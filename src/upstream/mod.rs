pub(crate) mod types;

mod anthropic;
mod openai;

use std::collections::HashMap;
use tracing::info;

use crate::config::{RoleConfig, UpstreamConfig};
use types::{ClientError, ClientResult};

// Re-export for agent use.
pub(crate) use types::Client;

/// Registry of upstream model providers. Created from config, used by Agent
/// to obtain model clients for each role.
pub(crate) struct Upstream {
    configs: HashMap<String, UpstreamConfig>,
}

impl Upstream {
    pub(crate) fn new(configs: &HashMap<String, UpstreamConfig>) -> Self {
        info!(count = configs.len(), "upstream registry created");
        Self {
            configs: configs.clone(),
        }
    }

    /// Get the raw config for an upstream by name.
    pub(crate) fn get_config(&self, name: &str) -> Option<&UpstreamConfig> {
        self.configs.get(name)
    }

    /// Create a model client for a specific agent role.
    pub(crate) fn client_for(&self, role: &RoleConfig) -> ClientResult<Box<dyn Client>> {
        if role.upstream.is_empty() {
            return Err(ClientError::Other(
                "no upstream configured for this role".into(),
            ));
        }
        let upstream_cfg = self
            .configs
            .get(&role.upstream)
            .ok_or_else(|| ClientError::Other(format!("upstream '{}' not found", role.upstream)))?;

        let client = create_provider_client(upstream_cfg, role)?;
        info!(
            upstream = role.upstream,
            provider = upstream_cfg.provider_type,
            model = role.model_name,
            "model client created"
        );
        Ok(client)
    }
}

fn create_provider_client(
    upstream: &UpstreamConfig,
    role: &RoleConfig,
) -> ClientResult<Box<dyn Client>> {
    match upstream.provider_type.as_str() {
        "openai" => Ok(Box::new(openai::OpenAiClient::new(upstream, role))),
        "anthropic" => Ok(Box::new(anthropic::AnthropicClient::new(upstream, role))),
        "gemini" => {
            info!("gemini: using OpenAI-compatible endpoint");
            let mut gemini_upstream = upstream.clone();
            if gemini_upstream.base_url.is_empty() {
                gemini_upstream.base_url =
                    "https://generativelanguage.googleapis.com/v1beta/openai".into();
            }
            Ok(Box::new(openai::OpenAiClient::new(&gemini_upstream, role)))
        }
        other => Err(ClientError::Other(format!(
            "unknown provider type: {other}"
        ))),
    }
}
