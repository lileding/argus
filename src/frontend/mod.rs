mod feishu;

use std::sync::Arc;
use tracing::{info, warn};

use crate::agent::{Agent, MessageSink};
use crate::config::Config;
use crate::server::Server;

/// A frontend is a Server that can receive Messages from the Agent.
pub trait Frontend: Server + MessageSink {}

/// Create all frontends defined in config. Returns (name, handle) pairs.
pub fn create_all(config: &Config, agent: &Arc<Agent>) -> Vec<(String, Arc<dyn Frontend>)> {
    let mut frontends = Vec::new();

    for (name, cfg) in &config.frontend {
        match name.as_str() {
            "feishu" => {
                if cfg.app_id.is_empty() || cfg.app_secret.is_empty() {
                    warn!(frontend = name, "skipping: empty app_id or app_secret");
                    continue;
                }
                let f = feishu::Feishu::new(Arc::clone(agent), cfg, &config.workspace_dir);
                info!(frontend = name, "frontend created");
                frontends.push((name.clone(), f as Arc<dyn Frontend>));
            }
            other => {
                warn!(frontend = other, "unknown frontend type, skipping");
            }
        }
    }

    frontends
}
