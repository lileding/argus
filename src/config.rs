use std::collections::HashMap;
use std::path::{Path, PathBuf};

use serde::Deserialize;

/// Subdirectory under workspace for downloaded media files.
pub(crate) const MEDIA_DIR: &str = ".files";

/// Application configuration loaded from TOML.
#[derive(Debug, Deserialize)]
pub(crate) struct Config {
    /// Workspace directory. Supports ~ and relative paths; resolved to
    /// absolute on load.
    #[serde(default = "default_workspace_dir")]
    pub(crate) workspace_dir: PathBuf,
    /// Named IM adapters. Key is the IM type ("feishu", "slack", etc.).
    #[serde(default)]
    pub(crate) gateway: HashMap<String, GatewayImConfig>,
    #[serde(default)]
    pub(crate) agent: AgentConfig,
    /// Named upstream model providers.
    #[serde(default)]
    pub(crate) upstream: HashMap<String, UpstreamConfig>,
    /// Database connection.
    #[serde(default)]
    pub(crate) database: DatabaseConfig,
}

#[derive(Debug, Clone, Deserialize, Default)]
pub(crate) struct DatabaseConfig {
    #[serde(default)]
    pub(crate) dsn: String,
}

#[derive(Debug, Clone, Deserialize)]
pub(crate) struct EmbeddingConfig {
    /// Which upstream provides the embedding endpoint.
    #[serde(default)]
    pub(crate) upstream: String,
    #[serde(default = "default_embedding_model")]
    pub(crate) model_name: String,
    #[serde(default = "default_batch_size")]
    pub(crate) batch_size: usize,
    #[serde(default = "default_interval_secs")]
    pub(crate) interval_secs: u64,
}

impl Default for EmbeddingConfig {
    fn default() -> Self {
        Self {
            upstream: String::new(),
            model_name: default_embedding_model(),
            batch_size: default_batch_size(),
            interval_secs: default_interval_secs(),
        }
    }
}

fn default_embedding_model() -> String {
    "modernbert-embed-base".into()
}
fn default_batch_size() -> usize {
    32
}
fn default_interval_secs() -> u64 {
    30
}

/// Frontend config. The HashMap key is the frontend type ("feishu", etc.).
/// Currently feishu-only; when adding other frontends, use an enum or
/// two-pass deserialization for type-specific fields.
#[derive(Debug, Clone, Deserialize)]
pub(crate) struct GatewayImConfig {
    #[serde(default)]
    pub(crate) app_id: String,
    #[serde(default)]
    pub(crate) app_secret: String,
    #[serde(default = "default_feishu_base_url")]
    pub(crate) base_url: String,
}

#[derive(Debug, Clone, Deserialize)]
pub(crate) struct AgentConfig {
    #[serde(default = "default_max_iterations")]
    #[allow(dead_code)] // Used when tool loop is implemented.
    pub(crate) max_iterations: usize,
    #[serde(default = "default_context_window")]
    pub(crate) orchestrator_context_window: usize,
    #[serde(default)]
    pub(crate) orchestrator: RoleConfig,
    #[serde(default)]
    pub(crate) synthesizer: RoleConfig,
    #[serde(default)]
    #[allow(dead_code)] // Used when whisper transcription is added.
    pub(crate) transcription: RoleConfig,
    #[serde(default)]
    pub(crate) embedding: EmbeddingConfig,
}

impl Default for AgentConfig {
    fn default() -> Self {
        Self {
            embedding: EmbeddingConfig::default(),
            max_iterations: default_max_iterations(),
            orchestrator_context_window: default_context_window(),
            orchestrator: RoleConfig::default(),
            synthesizer: RoleConfig::default(),
            transcription: RoleConfig::default(),
        }
    }
}

#[derive(Debug, Clone, Deserialize)]
pub(crate) struct RoleConfig {
    #[serde(default)]
    pub(crate) upstream: String,
    #[serde(default)]
    pub(crate) model_name: String,
    #[serde(default = "default_max_tokens")]
    pub(crate) max_tokens: usize,
}

impl Default for RoleConfig {
    fn default() -> Self {
        Self {
            upstream: String::new(),
            model_name: String::new(),
            max_tokens: default_max_tokens(),
        }
    }
}

#[derive(Debug, Clone, Deserialize)]
pub(crate) struct UpstreamConfig {
    #[serde(rename = "type")]
    pub(crate) provider_type: String,
    #[serde(default)]
    pub(crate) base_url: String,
    #[serde(default)]
    pub(crate) api_key: String,
    #[serde(default = "default_timeout")]
    pub(crate) timeout_secs: u64,
}

impl UpstreamConfig {
    pub(crate) fn effective_base_url(&self) -> &str {
        if !self.base_url.is_empty() {
            return &self.base_url;
        }
        match self.provider_type.as_str() {
            "openai" => "https://api.openai.com/v1",
            "anthropic" => "https://api.anthropic.com",
            "gemini" => "https://generativelanguage.googleapis.com",
            _ => "",
        }
    }
}

// --- Defaults ---

fn default_workspace_dir() -> PathBuf {
    PathBuf::from("~/.local/share/argus")
}

fn default_feishu_base_url() -> String {
    "https://open.feishu.cn".into()
}

fn default_max_iterations() -> usize {
    10
}

fn default_context_window() -> usize {
    10
}

fn default_max_tokens() -> usize {
    4096
}

fn default_timeout() -> u64 {
    120
}

// --- Loading ---

impl Config {
    /// Load config from TOML file. Falls back to defaults if file doesn't exist.
    /// Resolves workspace_dir to absolute (expand ~, join with cwd if relative).
    pub(crate) fn load(config_path: &Path) -> anyhow::Result<Self> {
        let mut config: Config = if config_path.exists() {
            let content = std::fs::read_to_string(config_path)?;
            let c: Config = toml::from_str(&content)?;
            tracing::info!(path = %config_path.display(), "config loaded");
            c
        } else {
            tracing::info!(
                path = %config_path.display(),
                "config file not found, using defaults"
            );
            toml::from_str("").unwrap()
        };

        // Resolve workspace_dir to absolute (expand ~, join with cwd if relative).
        let ws_str = config
            .workspace_dir
            .to_str()
            .ok_or_else(|| anyhow::anyhow!("workspace_dir contains invalid UTF-8"))?;
        config.workspace_dir = resolve_path(ws_str)?;
        std::fs::create_dir_all(&config.workspace_dir)?;

        tracing::info!(workspace = %config.workspace_dir.display(), "workspace resolved");
        Ok(config)
    }
}

/// Expand ~ and resolve relative paths to absolute using cwd.
fn resolve_path(path: &str) -> anyhow::Result<PathBuf> {
    let expanded = expand_tilde(path);
    if expanded.is_absolute() {
        Ok(expanded)
    } else {
        Ok(std::env::current_dir()?.join(expanded))
    }
}

pub(crate) fn expand_tilde(path: &str) -> PathBuf {
    if let Some(rest) = path.strip_prefix("~/") {
        if let Some(home) = dirs_home() {
            return home.join(rest);
        }
    } else if path == "~"
        && let Some(home) = dirs_home()
    {
        return home;
    }
    PathBuf::from(path)
}

fn dirs_home() -> Option<PathBuf> {
    std::env::var_os("HOME").map(PathBuf::from)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_minimal() {
        let config: Config = toml::from_str("").unwrap();
        assert_eq!(config.workspace_dir, PathBuf::from("~/.local/share/argus"));
        assert!(config.gateway.is_empty());
        assert!(config.upstream.is_empty());
        assert_eq!(config.agent.max_iterations, 10);
    }

    #[test]
    fn parse_full() {
        let config: Config = toml::from_str(
            r#"
workspace_dir = "/data/argus"

[gateway.feishu]
app_id = "cli_abc"
app_secret = "secret123"

[agent]
max_iterations = 15
orchestrator_context_window = 8

[agent.orchestrator]
upstream = "anthropic"
model_name = "claude-haiku-4-5"

[agent.synthesizer]
upstream = "gemini"
model_name = "gemini-2.5-flash-lite"
max_tokens = 32768

[upstream.local]
type = "openai"
base_url = "http://localhost:8000/v1"
api_key = "omlx"
timeout_secs = 240

[upstream.anthropic]
type = "anthropic"
api_key = "sk-ant-xxx"
"#,
        )
        .unwrap();

        assert_eq!(config.workspace_dir, PathBuf::from("/data/argus"));

        let feishu = config.gateway.get("feishu").unwrap();
        assert_eq!(feishu.app_id, "cli_abc");
        assert_eq!(feishu.base_url, "https://open.feishu.cn");

        assert_eq!(config.agent.max_iterations, 15);
        assert_eq!(config.agent.orchestrator.upstream, "anthropic");
        assert_eq!(config.agent.synthesizer.max_tokens, 32768);

        let local = config.upstream.get("local").unwrap();
        assert_eq!(local.provider_type, "openai");
        assert_eq!(local.timeout_secs, 240);

        let anthropic = config.upstream.get("anthropic").unwrap();
        assert_eq!(anthropic.timeout_secs, 120);
    }

    #[test]
    fn parse_multiple_frontends() {
        let config: Config = toml::from_str(
            r#"
[gateway.feishu]
app_id = "abc"
app_secret = "secret"

[gateway.slack]
app_id = "xoxb-xxx"
"#,
        )
        .unwrap();
        assert_eq!(config.gateway.len(), 2);
    }

    #[test]
    fn effective_base_url_explicit() {
        let up = UpstreamConfig {
            provider_type: "openai".into(),
            base_url: "http://custom:8080/v1".into(),
            api_key: String::new(),
            timeout_secs: 60,
        };
        assert_eq!(up.effective_base_url(), "http://custom:8080/v1");
    }

    #[test]
    fn effective_base_url_defaults() {
        let make = |t: &str| UpstreamConfig {
            provider_type: t.into(),
            base_url: String::new(),
            api_key: String::new(),
            timeout_secs: 60,
        };
        assert_eq!(
            make("openai").effective_base_url(),
            "https://api.openai.com/v1"
        );
        assert_eq!(
            make("anthropic").effective_base_url(),
            "https://api.anthropic.com"
        );
        assert!(make("gemini").effective_base_url().contains("googleapis"));
    }

    #[test]
    fn expand_tilde_with_subpath() {
        let result = expand_tilde("~/foo/bar");
        assert!(result.to_str().unwrap().ends_with("/foo/bar"));
        assert!(!result.to_str().unwrap().starts_with("~"));
    }

    #[test]
    fn expand_tilde_bare() {
        let result = expand_tilde("~");
        assert!(!result.to_str().unwrap().starts_with("~"));
    }

    #[test]
    fn expand_tilde_absolute_passthrough() {
        assert_eq!(expand_tilde("/abs/path"), PathBuf::from("/abs/path"));
    }

    #[test]
    fn resolve_path_absolute() {
        let p = resolve_path("/abs/path").unwrap();
        assert_eq!(p, PathBuf::from("/abs/path"));
    }

    #[test]
    fn resolve_path_relative() {
        let p = resolve_path("relative/dir").unwrap();
        assert!(p.is_absolute());
        assert!(p.to_str().unwrap().ends_with("relative/dir"));
    }
}
