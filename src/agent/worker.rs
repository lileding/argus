//! Background workers: embedder and summarizer.
//! Spawned by Agent, run until cancelled.

use std::sync::Arc;
use std::time::Duration;

use tokio_util::sync::CancellationToken;
use tracing::{debug, info, warn};

use super::embedding::EmbeddingClient;
use crate::database::Database;
use crate::upstream;
use crate::upstream::types as model;

const SUMMARY_PROMPT: &str = "Summarize the following assistant reply in 2-3 concise sentences. \
Preserve the key facts, conclusions, and any specific data points. \
Use the same language as the original. Output ONLY the summary, nothing else.";

/// Spawn the embedder background task. Returns its join handle.
pub(super) fn spawn_embedder(
    db: Arc<Database>,
    embedder: Arc<EmbeddingClient>,
    cancel: CancellationToken,
    batch_size: usize,
    interval: Duration,
) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        info!("embedder worker started");
        loop {
            tokio::select! {
                _ = tokio::time::sleep(interval) => {
                    embed_cycle(&db, &embedder, batch_size).await;
                }
                _ = cancel.cancelled() => break,
            }
        }
        info!("embedder worker stopped");
    })
}

/// Spawn the summarizer background task. Returns its join handle.
pub(super) fn spawn_summarizer(
    db: Arc<Database>,
    synthesizer: Arc<dyn upstream::Client>,
    cancel: CancellationToken,
    interval: Duration,
) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        info!("summarizer worker started");
        loop {
            tokio::select! {
                _ = tokio::time::sleep(interval) => {
                    summarize_cycle(&db, &*synthesizer).await;
                }
                _ = cancel.cancelled() => break,
            }
        }
        info!("summarizer worker stopped");
    })
}

/// Spawn the document ingester background task.
pub(super) fn spawn_ingester(
    db: Arc<Database>,
    cancel: CancellationToken,
    interval: Duration,
) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        info!("ingester worker started");
        loop {
            tokio::select! {
                _ = tokio::time::sleep(interval) => {
                    ingest_cycle(&db).await;
                }
                _ = cancel.cancelled() => break,
            }
        }
        info!("ingester worker stopped");
    })
}

/// One ingestion cycle: process pending large documents.
async fn ingest_cycle(db: &Database) {
    let docs = match db.documents.pending(5).await {
        Ok(d) => d,
        Err(e) => {
            warn!(error = %e, "fetch pending documents failed");
            return;
        }
    };
    for doc in &docs {
        if let Err(e) = super::docindex::ingest_document(
            db,
            doc.id,
            &doc.filename,
            std::path::Path::new(&doc.file_path),
        )
        .await
        {
            warn!(doc_id = doc.id, filename = doc.filename, error = %e, "document ingestion failed");
            let _ = db
                .documents
                .update_status(doc.id, "error", &e.to_string())
                .await;
        }
    }
}

/// One embedding cycle: fetch unembedded messages + notifications + chunks, embed, write back.
async fn embed_cycle(db: &Database, embedder: &EmbeddingClient, batch_size: usize) {
    embed_table(&db.messages, embedder, batch_size, "messages").await;
    embed_table(&db.notifications, embedder, batch_size, "notifications").await;
    embed_table(&db.documents, embedder, batch_size, "chunks").await;
}

/// Generic embed helper for any table that has unembedded() + set_embedding().
async fn embed_table(
    table: &(impl Embeddable + Sync),
    embedder: &EmbeddingClient,
    batch_size: usize,
    label: &str,
) {
    let rows = match table.unembedded(batch_size as i64).await {
        Ok(r) if !r.is_empty() => r,
        _ => return,
    };

    let texts: Vec<&str> = rows.iter().map(|(_, c)| c.as_str()).collect();
    match embedder.embed_batch(&texts).await {
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
    async fn unembedded(&self, limit: i64) -> anyhow::Result<Vec<(i64, String)>>;
    async fn set_embedding(&self, id: i64, embedding: &[f32]) -> anyhow::Result<()>;
}

#[async_trait::async_trait]
impl Embeddable for crate::database::Messages {
    async fn unembedded(&self, limit: i64) -> anyhow::Result<Vec<(i64, String)>> {
        self.unembedded(limit).await
    }
    async fn set_embedding(&self, id: i64, embedding: &[f32]) -> anyhow::Result<()> {
        self.set_embedding(id, embedding).await
    }
}

#[async_trait::async_trait]
impl Embeddable for crate::database::Documents {
    async fn unembedded(&self, limit: i64) -> anyhow::Result<Vec<(i64, String)>> {
        self.unembedded_chunks(limit).await
    }
    async fn set_embedding(&self, id: i64, embedding: &[f32]) -> anyhow::Result<()> {
        self.set_chunk_embedding(id, embedding).await
    }
}

#[async_trait::async_trait]
impl Embeddable for crate::database::Notifications {
    async fn unembedded(&self, limit: i64) -> anyhow::Result<Vec<(i64, String)>> {
        self.unembedded(limit).await
    }
    async fn set_embedding(&self, id: i64, embedding: &[f32]) -> anyhow::Result<()> {
        self.set_embedding(id, embedding).await
    }
}

/// One summarization cycle: fetch long unsummarized notifications, summarize, write back.
async fn summarize_cycle(db: &Database, synthesizer: &dyn upstream::Client) {
    let rows = match db.notifications.unsummarized(5).await {
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
        match synthesizer.chat(&messages, &[]).await {
            Ok(resp) => {
                if let Err(e) = db.notifications.set_summary(*id, &resp.content).await {
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
