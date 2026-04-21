pub mod types;

mod anthropic;
mod openai;
// mod retry;      // TODO

use std::sync::Arc;
use tracing::info;

use crate::config::{Config, RoleConfig, UpstreamConfig};
use types::{ChunkStream, ClientError, ClientResult, Message, Response, ToolDef};

/// Model client interface. Each provider implements this.
#[async_trait::async_trait]
pub trait Client: Send + Sync {
    /// Non-streaming chat completion.
    async fn chat(&self, messages: &[Message], tools: &[ToolDef]) -> ClientResult<Response>;

    /// Streaming chat completion. Returns an async stream of chunks.
    async fn chat_stream(
        &self,
        messages: &[Message],
        tools: &[ToolDef],
    ) -> ClientResult<ChunkStream>;

    /// Streaming chat that aborts early if the model produces more than
    /// `max_text_tokens` of text content without any tool calls.
    /// Returns a fully accumulated Response.
    async fn chat_with_early_abort(
        &self,
        messages: &[Message],
        tools: &[ToolDef],
        max_text_tokens: usize,
    ) -> ClientResult<Response>;
}

/// Create a model client for a given agent role from config.
pub fn create_client(config: &Config, role: &RoleConfig) -> ClientResult<Arc<dyn Client>> {
    if role.upstream.is_empty() {
        return Err(ClientError::Other(
            "no upstream configured for this role".into(),
        ));
    }
    let upstream = config.upstream.get(&role.upstream).ok_or_else(|| {
        ClientError::Other(format!("upstream '{}' not found in config", role.upstream))
    })?;

    let client = create_provider_client(upstream, role)?;
    info!(
        upstream = role.upstream,
        provider = upstream.provider_type,
        model = role.model_name,
        "model client created"
    );
    Ok(client)
}

fn create_provider_client(
    upstream: &UpstreamConfig,
    role: &RoleConfig,
) -> ClientResult<Arc<dyn Client>> {
    match upstream.provider_type.as_str() {
        "openai" => Ok(Arc::new(openai::OpenAiClient::new(upstream, role))),
        "anthropic" => Ok(Arc::new(anthropic::AnthropicClient::new(upstream, role))),
        "gemini" => {
            // Gemini supports OpenAI-compatible endpoint.
            // Use OpenAI client with Gemini's compatibility base URL.
            info!("gemini: using OpenAI-compatible endpoint");
            let mut gemini_upstream = upstream.clone();
            if gemini_upstream.base_url.is_empty() {
                gemini_upstream.base_url =
                    "https://generativelanguage.googleapis.com/v1beta/openai".into();
            }
            Ok(Arc::new(openai::OpenAiClient::new(&gemini_upstream, role)))
        }
        other => Err(ClientError::Other(format!(
            "unknown provider type: {other}"
        ))),
    }
}
