use std::collections::HashMap;
use std::path::{Path, PathBuf};

use serde::Deserialize;

/// Subdirectory under workspace for downloaded media files.
pub const MEDIA_DIR: &str = ".files";

/// Application configuration. Loaded from TOML, with `workspace_dir`
/// injected from CLI args (not present in the config file).
#[derive(Debug)]
#[allow(dead_code)] // Fields consumed incrementally as features are built.
pub struct Config {
    /// Workspace directory (from --workspace CLI arg).
    pub workspace_dir: PathBuf,
    /// Named frontend instances (e.g. "feishu").
    pub frontend: HashMap<String, FrontendConfig>,
    /// Agent settings.
    pub agent: AgentConfig,
    /// Named upstream model providers (e.g. "openai", "anthropic").
    pub upstream: HashMap<String, UpstreamConfig>,
}

/// Raw TOML structure (without workspace_dir).
#[derive(Debug, Deserialize)]
struct RawConfig {
    #[serde(default)]
    frontend: HashMap<String, FrontendConfig>,
    #[serde(default)]
    agent: AgentConfig,
    #[serde(default)]
    upstream: HashMap<String, UpstreamConfig>,
}

#[derive(Debug, Clone, Deserialize)]
#[allow(dead_code)]
pub struct FrontendConfig {
    /// Frontend type: "feishu" (future: "slack", "cli").
    #[serde(rename = "type")]
    pub frontend_type: String,
    #[serde(default)]
    pub app_id: String,
    #[serde(default)]
    pub app_secret: String,
    /// Feishu API base URL.
    #[serde(default = "default_feishu_base_url")]
    pub base_url: String,
}

#[derive(Debug, Clone, Deserialize)]
#[allow(dead_code)]
pub struct AgentConfig {
    #[serde(default = "default_max_iterations")]
    pub max_iterations: usize,
    #[serde(default = "default_context_window")]
    pub orchestrator_context_window: usize,
    #[serde(default)]
    pub orchestrator: RoleConfig,
    #[serde(default)]
    pub synthesizer: RoleConfig,
    #[serde(default)]
    pub transcription: RoleConfig,
}

impl Default for AgentConfig {
    fn default() -> Self {
        Self {
            max_iterations: default_max_iterations(),
            orchestrator_context_window: default_context_window(),
            orchestrator: RoleConfig::default(),
            synthesizer: RoleConfig::default(),
            transcription: RoleConfig::default(),
        }
    }
}

/// Which upstream and model to use for a given agent role.
#[derive(Debug, Clone, Deserialize)]
#[allow(dead_code)]
pub struct RoleConfig {
    /// Name of the upstream (key into Config.upstream).
    #[serde(default)]
    pub upstream: String,
    #[serde(default)]
    pub model_name: String,
    #[serde(default = "default_max_tokens")]
    pub max_tokens: usize,
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

/// A named model provider backend.
#[derive(Debug, Clone, Deserialize)]
#[allow(dead_code)]
pub struct UpstreamConfig {
    /// Provider type: "openai", "anthropic", "gemini".
    #[serde(rename = "type")]
    pub provider_type: String,
    #[serde(default)]
    pub base_url: String,
    #[serde(default)]
    pub api_key: String,
    #[serde(default = "default_timeout")]
    pub timeout_secs: u64,
}

impl UpstreamConfig {
    /// Returns base_url if set, otherwise the default for the provider type.
    #[allow(dead_code)]
    pub fn effective_base_url(&self) -> &str {
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
    /// Load config from TOML file + CLI workspace_dir.
    /// Falls back to defaults if file doesn't exist.
    pub fn load(config_path: &Path, workspace_dir: PathBuf) -> anyhow::Result<Self> {
        let raw = if config_path.exists() {
            let content = std::fs::read_to_string(config_path)?;
            let raw: RawConfig = toml::from_str(&content)?;
            tracing::info!(path = %config_path.display(), "config loaded");
            raw
        } else {
            tracing::info!(
                path = %config_path.display(),
                "config file not found, using defaults"
            );
            RawConfig {
                frontend: HashMap::new(),
                agent: AgentConfig::default(),
                upstream: HashMap::new(),
            }
        };

        Ok(Config {
            workspace_dir,
            frontend: raw.frontend,
            agent: raw.agent,
            upstream: raw.upstream,
        })
    }

    /// Resolve the upstream config for a role.
    #[allow(dead_code)]
    pub fn upstream_for(&self, role: &RoleConfig) -> Option<&UpstreamConfig> {
        if role.upstream.is_empty() {
            return None;
        }
        self.upstream.get(&role.upstream)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parse(toml_str: &str) -> Config {
        let raw: RawConfig = toml::from_str(toml_str).unwrap();
        Config {
            workspace_dir: PathBuf::from("/test"),
            frontend: raw.frontend,
            agent: raw.agent,
            upstream: raw.upstream,
        }
    }

    #[test]
    fn parse_minimal() {
        let config = parse("");
        assert_eq!(config.agent.max_iterations, 10);
        assert!(config.frontend.is_empty());
        assert!(config.upstream.is_empty());
    }

    #[test]
    fn parse_full() {
        let config = parse(
            r#"
[frontend.feishu]
type = "feishu"
app_id = "cli_abc"
app_secret = "secret123"

[agent]
max_iterations = 15
orchestrator_context_window = 8

[agent.orchestrator]
upstream = "anthropic"
model_name = "claude-haiku-4-5"
max_tokens = 4096

[agent.synthesizer]
upstream = "gemini"
model_name = "gemini-2.5-flash-lite"
max_tokens = 32768

[agent.transcription]
upstream = "local"
model_name = "whisper-large-v3"

[upstream.local]
type = "openai"
base_url = "http://localhost:8000/v1"
api_key = "omlx"
timeout_secs = 240

[upstream.anthropic]
type = "anthropic"
api_key = "sk-ant-xxx"

[upstream.gemini]
type = "gemini"
api_key = "AIza-xxx"
"#,
        );

        let feishu = config.frontend.get("feishu").unwrap();
        assert_eq!(feishu.frontend_type, "feishu");
        assert_eq!(feishu.app_id, "cli_abc");
        assert_eq!(feishu.base_url, "https://open.feishu.cn"); // default

        assert_eq!(config.agent.max_iterations, 15);
        assert_eq!(config.agent.orchestrator.upstream, "anthropic");
        assert_eq!(config.agent.orchestrator.model_name, "claude-haiku-4-5");
        assert_eq!(config.agent.synthesizer.max_tokens, 32768);
        assert_eq!(config.agent.transcription.model_name, "whisper-large-v3");

        let local = config.upstream.get("local").unwrap();
        assert_eq!(local.provider_type, "openai");
        assert_eq!(local.base_url, "http://localhost:8000/v1");
        assert_eq!(local.timeout_secs, 240);

        let anthropic = config.upstream.get("anthropic").unwrap();
        assert_eq!(anthropic.timeout_secs, 120); // default

        assert!(config.upstream_for(&config.agent.orchestrator).is_some());
        assert!(config.upstream_for(&RoleConfig::default()).is_none());
    }

    #[test]
    fn parse_multiple_frontends() {
        let config = parse(
            r#"
[frontend.feishu]
type = "feishu"
app_id = "cli_abc"
app_secret = "secret"

[frontend.slack]
type = "slack"
app_id = "xoxb-xxx"
"#,
        );
        assert_eq!(config.frontend.len(), 2);
        assert_eq!(config.frontend["feishu"].frontend_type, "feishu");
        assert_eq!(config.frontend["slack"].frontend_type, "slack");
    }

    #[test]
    fn defaults_are_sane() {
        let config = parse("");
        assert_eq!(config.workspace_dir, PathBuf::from("/test"));
        assert_eq!(config.agent.max_iterations, 10);
        assert_eq!(config.agent.orchestrator_context_window, 10);
        assert_eq!(config.agent.orchestrator.max_tokens, 4096);
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
}
