//! Background indexer: embedding, summarization, document ingestion.
//!
//! Runs as a peer service alongside Agent and Gateway. Single-loop design:
//! embed + ingest every tick, summarize every N ticks.

mod client;
pub(crate) mod docindex;

use std::time::Duration;

use tokio_util::sync::CancellationToken;
use tracing::{debug, info, warn};

use crate::config::EmbedderConfig;
use crate::database::Database;
use crate::upstream;
use crate::upstream::types as model;
use client::EmbeddingClient;

// Re-export for gateway.
pub(crate) use docindex::process_upload;

const SUMMARY_PROMPT: &str = "Summarize the following assistant reply in 2-3 concise sentences. \
Preserve the key facts, conclusions, and any specific data points. \
Use the same language as the original. Output ONLY the summary, nothing else.";

#[derive(Debug, thiserror::Error)]
pub(crate) enum EmbedderError {
    #[error("no embedding upstream configured")]
    NoUpstream,
    #[error("upstream error: {0}")]
    UpstreamError(#[from] upstream::types::ClientError),
}

/// Background indexer: embedding, summarization, document ingestion.
pub(crate) struct Embedder<'a> {
    db: &'a Database,
    client: EmbeddingClient,
    summarizer: Box<dyn upstream::Client>,
    batch_size: usize,
    interval: Duration,
    summary_interval_ticks: Duration,
}

impl<'a> Embedder<'a> {
    pub(crate) fn new(
        config: &EmbedderConfig,
        upstream_reg: &upstream::Upstream,
        db: &'a Database,
    ) -> Result<Self, EmbedderError> {
        if config.upstream.is_empty() {
            return Err(EmbedderError::NoUpstream);
        }

        let up_cfg = upstream_reg.get_config(&config.upstream).ok_or_else(|| {
            upstream::types::ClientError::Other(format!(
                "embedder upstream '{}' not found",
                config.upstream
            ))
        })?;
        let base_url = up_cfg.effective_base_url();
        let client = EmbeddingClient::new(base_url, &up_cfg.api_key, &config.model_name);
        let summarizer = upstream_reg.client_for(&config.summarizer)?;

        info!(
            model = config.model_name,
            interval_secs = config.interval_secs,
            summary_every = config.summary_interval_ticks,
            "embedder initialized"
        );

        Ok(Self {
            db,
            client,
            summarizer,
            batch_size: config.batch_size,
            interval: Duration::from_secs(config.interval_secs),
            summary_interval_ticks: Duration::from_secs(config.summary_interval_ticks),
        })
    }
}

// Implement agent's EmbedService trait so main can wire them together.
#[async_trait::async_trait]
impl crate::agent::EmbedService for Embedder<'_> {
    async fn embed_one(
        &self,
        text: &str,
    ) -> Result<Vec<f32>, Box<dyn std::error::Error + Send + Sync>> {
        Ok(self.client.embed_one(text).await?)
    }

    fn model_name(&self) -> &str {
        self.client.model_name()
    }
}

// --- Cycle functions ---

impl Embedder<'_> {
    pub(crate) async fn run(&self, cancel: &CancellationToken) {
        info!("embedder started");
        let mut embed_interval = tokio::time::interval(self.interval);
        let mut summary_interval = tokio::time::interval(self.summary_interval_ticks);
        loop {
            tokio::select! {
                _ = embed_interval.tick() => {
                    self.embed_cycle().await;
                    self.ingest_cycle().await;
                },
                _ = summary_interval.tick() => {
                    self.summarize_cycle().await;
                },
                _ = cancel.cancelled() => break,
            }
        }
        info!("embedder stopped");
    }

    async fn embed_cycle(&self) {
        embed_table(&self.db.messages, &self.client, self.batch_size, "messages").await;
        embed_table(
            &self.db.notifications,
            &self.client,
            self.batch_size,
            "notifications",
        )
        .await;
        embed_table(&self.db.documents, &self.client, self.batch_size, "chunks").await;
        embed_table(&self.db.memories, &self.client, self.batch_size, "memories").await;
    }

    async fn ingest_cycle(&self) {
        let docs = match self.db.documents.pending(5).await {
            Ok(d) => d,
            Err(e) => {
                warn!(error = %e, "fetch pending documents failed");
                return;
            }
        };
        for doc in &docs {
            if let Err(e) = docindex::ingest_document(
                self.db,
                doc.id,
                &doc.filename,
                std::path::Path::new(&doc.file_path),
            )
            .await
            {
                warn!(doc_id = doc.id, filename = doc.filename, error = %e, "document ingestion failed");
                let _ = self
                    .db
                    .documents
                    .update_status(doc.id, "error", &e.to_string())
                    .await;
            }
        }
    }

    async fn summarize_cycle(&self) {
        let rows = match self.db.notifications.unsummarized(5).await {
            Ok(r) => r,
            Err(e) => {
                warn!(error = %e, "fetch unsummarized failed");
                return;
            }
        };

        for (id, content) in &rows {
            let messages = vec![
                model::Message::system(SUMMARY_PROMPT),
                model::Message::user(content.as_str()),
            ];
            match self
                .summarizer
                .chat(
                    &messages,
                    &[],
                    &crate::upstream::types::ChatOptions::default(),
                )
                .await
            {
                Ok(resp) => {
                    if let Err(e) = self.db.notifications.set_summary(*id, &resp.content).await {
                        warn!(id, error = %e, "set summary failed");
                    }
                }
                Err(e) => {
                    warn!(id, error = %e, "summarize failed, will retry");
                }
            }
        }

        if !rows.is_empty() {
            debug!(count = rows.len(), "notifications summarized");
        }
    }
}

// --- Generic embedding helper ---

async fn embed_table(
    table: &(impl Embeddable + Sync),
    client: &EmbeddingClient,
    batch_size: usize,
    label: &str,
) {
    let rows = match table.unembedded(batch_size as i64).await {
        Ok(r) if !r.is_empty() => r,
        _ => return,
    };

    let texts: Vec<&str> = rows.iter().map(|(_, c)| c.as_str()).collect();
    match client.embed_batch(&texts).await {
        Ok(vecs) => {
            for ((id, _), vec) in rows.iter().zip(vecs.iter()) {
                if let Err(e) = table.set_embedding(*id, vec).await {
                    warn!(id, label, error = %e, "set embedding failed");
                }
            }
            debug!(count = rows.len(), label, "embedded");
        }
        Err(e) => warn!(label, error = %e, "batch embed failed"),
    }
}

/// Trait for tables that support embedding.
#[async_trait::async_trait]
trait Embeddable {
    async fn unembedded(&self, limit: i64) -> crate::database::DbResult<Vec<(i64, String)>>;
    async fn set_embedding(&self, id: i64, embedding: &[f32]) -> crate::database::DbResult<()>;
}

#[async_trait::async_trait]
impl Embeddable for crate::database::Messages {
    async fn unembedded(&self, limit: i64) -> crate::database::DbResult<Vec<(i64, String)>> {
        self.unembedded(limit).await
    }
    async fn set_embedding(&self, id: i64, embedding: &[f32]) -> crate::database::DbResult<()> {
        self.set_embedding(id, embedding).await
    }
}

#[async_trait::async_trait]
impl Embeddable for crate::database::Memories {
    async fn unembedded(&self, limit: i64) -> crate::database::DbResult<Vec<(i64, String)>> {
        self.unembedded(limit).await
    }
    async fn set_embedding(&self, id: i64, embedding: &[f32]) -> crate::database::DbResult<()> {
        self.set_embedding(id, embedding).await
    }
}

#[async_trait::async_trait]
impl Embeddable for crate::database::Documents {
    async fn unembedded(&self, limit: i64) -> crate::database::DbResult<Vec<(i64, String)>> {
        self.unembedded_chunks(limit).await
    }
    async fn set_embedding(&self, id: i64, embedding: &[f32]) -> crate::database::DbResult<()> {
        self.set_chunk_embedding(id, embedding).await
    }
}

#[async_trait::async_trait]
impl Embeddable for crate::database::Notifications {
    async fn unembedded(&self, limit: i64) -> crate::database::DbResult<Vec<(i64, String)>> {
        self.unembedded(limit).await
    }
    async fn set_embedding(&self, id: i64, embedding: &[f32]) -> crate::database::DbResult<()> {
        self.set_embedding(id, embedding).await
    }
}
