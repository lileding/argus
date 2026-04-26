//! Audio transcription: calls an OpenAI-compatible /v1/audio/transcriptions endpoint.

use serde::Deserialize;
use tracing::debug;

/// Domain-specific vocabulary hints for Whisper.
const TRANSCRIPTION_PROMPT: &str = "API, Kubernetes, Docker, GPU, LLM, transformer, embedding, \
ETF, hedge fund, quantitative, 莫扎特, 贝多芬, 巴赫, 肖邦, \
sonata, concerto, fugue, symphony, étude, nocturne, prelude";

#[derive(Debug, thiserror::Error)]
pub(super) enum TranscribeError {
    #[error("http: {0}")]
    Http(#[from] reqwest::Error),
    #[error("json: {0}")]
    Json(#[from] serde_json::Error),
    #[error("api error ({status}): {message}")]
    Api { status: u16, message: String },
}

pub(super) struct TranscribeResult {
    pub(super) text: String,
    #[allow(dead_code)] // Will be used for LLM correction threshold.
    pub(super) confidence: f64,
}

pub(super) struct TranscribeClient {
    http: reqwest::Client,
    url: String,
    api_key: String,
    model: String,
}

impl TranscribeClient {
    pub(super) fn new(base_url: &str, api_key: &str, model: &str) -> Self {
        Self {
            http: reqwest::Client::new(),
            url: format!("{}/audio/transcriptions", base_url.trim_end_matches('/')),
            api_key: api_key.to_string(),
            model: model.to_string(),
        }
    }

    pub(super) async fn transcribe(
        &self,
        audio: &[u8],
        filename: &str,
    ) -> Result<TranscribeResult, TranscribeError> {
        let file_part = reqwest::multipart::Part::bytes(audio.to_vec())
            .file_name(filename.to_string())
            .mime_str("audio/ogg")
            .unwrap();

        let form = reqwest::multipart::Form::new()
            .text("model", self.model.clone())
            .text("response_format", "verbose_json")
            .text("language", "zh")
            .text("prompt", TRANSCRIPTION_PROMPT)
            .part("file", file_part);

        let mut req = self.http.post(&self.url).multipart(form);
        if !self.api_key.is_empty() {
            req = req.bearer_auth(&self.api_key);
        }

        let resp = req.send().await?;
        let status = resp.status();
        let body = resp.text().await?;

        if !status.is_success() {
            return Err(TranscribeError::Api {
                status: status.as_u16(),
                message: body,
            });
        }

        let parsed: VerboseResponse = serde_json::from_str(&body)?;

        let confidence = if parsed.segments.is_empty() {
            0.0
        } else {
            parsed.segments.iter().map(|s| s.avg_logprob).sum::<f64>()
                / parsed.segments.len() as f64
        };

        debug!(
            text_len = parsed.text.len(),
            confidence, "transcription complete"
        );

        Ok(TranscribeResult {
            text: parsed.text,
            confidence,
        })
    }
}

#[derive(Deserialize)]
struct VerboseResponse {
    text: String,
    #[serde(default)]
    segments: Vec<Segment>,
}

#[derive(Deserialize)]
struct Segment {
    #[serde(default)]
    avg_logprob: f64,
}
