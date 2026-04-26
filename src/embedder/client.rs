//! Embedding client: calls an OpenAI-compatible /v1/embeddings endpoint.

use serde::{Deserialize, Serialize};
use tracing::debug;

/// Max characters per input. OpenAI text-embedding-3-* has an 8192 token limit;
/// for Chinese text ~2-3 tokens/char, so 2500 chars is a safe ceiling (~7500 tokens).
const MAX_EMBED_CHARS: usize = 2500;

/// Truncate a string to max chars, respecting UTF-8 boundaries.
fn truncate_chars(s: &str, max_chars: usize) -> String {
    if s.chars().count() <= max_chars {
        s.to_string()
    } else {
        s.chars().take(max_chars).collect()
    }
}

#[derive(Debug, thiserror::Error)]
pub(super) enum EmbedClientError {
    #[error("http: {0}")]
    Http(#[from] reqwest::Error),
    #[error("json: {0}")]
    Json(#[from] serde_json::Error),
    #[error("api error ({status}): {message}")]
    Api { status: u16, message: String },
    #[error("{0}")]
    Other(String),
}

pub(super) type EmbedResult<T> = Result<T, EmbedClientError>;

/// Embedding client for converting text → vector.
pub(super) struct EmbeddingClient {
    http: reqwest::Client,
    url: String,
    api_key: String,
    model: String,
    /// Optional output dimension (for OpenAI text-embedding-3-{small,large}).
    dimensions: Option<usize>,
}

impl EmbeddingClient {
    pub(super) fn model_name(&self) -> &str {
        &self.model
    }

    pub(super) fn new(
        base_url: &str,
        api_key: &str,
        model: &str,
        dimensions: Option<usize>,
    ) -> Self {
        Self {
            http: reqwest::Client::new(),
            url: format!("{}/embeddings", base_url.trim_end_matches('/')),
            api_key: api_key.to_string(),
            model: model.to_string(),
            dimensions,
        }
    }

    /// Embed a single text string.
    pub(super) async fn embed_one(&self, text: &str) -> EmbedResult<Vec<f32>> {
        let vecs = self.embed_batch(&[text]).await?;
        vecs.into_iter()
            .next()
            .ok_or_else(|| EmbedClientError::Other("empty embedding response".into()))
    }

    /// Embed a batch of text strings. Returns vectors in the same order.
    /// Inputs are truncated to MAX_EMBED_CHARS to stay within token limits.
    pub(super) async fn embed_batch(&self, texts: &[&str]) -> EmbedResult<Vec<Vec<f32>>> {
        let truncated: Vec<String> = texts
            .iter()
            .map(|t| truncate_chars(t, MAX_EMBED_CHARS))
            .collect();
        let truncated_refs: Vec<&str> = truncated.iter().map(|s| s.as_str()).collect();
        let body = EmbedRequest {
            model: &self.model,
            input: &truncated_refs,
            dimensions: self.dimensions,
        };

        let mut req = self.http.post(&self.url).json(&body);
        if !self.api_key.is_empty() {
            req = req.bearer_auth(&self.api_key);
        }

        let resp = req.send().await?;
        let status = resp.status();
        let text = resp.text().await?;

        if !status.is_success() {
            return Err(EmbedClientError::Api {
                status: status.as_u16(),
                message: text,
            });
        }

        let parsed: EmbedResponse = serde_json::from_str(&text)?;
        let mut result: Vec<(usize, Vec<f32>)> = parsed
            .data
            .into_iter()
            .map(|d| (d.index, d.embedding))
            .collect();
        // Sort by index to match input order.
        result.sort_by_key(|(i, _)| *i);

        debug!(count = result.len(), "embeddings received");
        Ok(result.into_iter().map(|(_, v)| v).collect())
    }
}

#[derive(Serialize)]
struct EmbedRequest<'a> {
    model: &'a str,
    input: &'a [&'a str],
    #[serde(skip_serializing_if = "Option::is_none")]
    dimensions: Option<usize>,
}

#[derive(Deserialize)]
struct EmbedResponse {
    data: Vec<EmbedData>,
}

#[derive(Deserialize)]
struct EmbedData {
    index: usize,
    embedding: Vec<f32>,
}
