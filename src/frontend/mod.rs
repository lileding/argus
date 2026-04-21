mod feishu;

use std::sync::Arc;
use tracing::{info, warn};

use std::collections::HashMap;
use std::path::Path;

use crate::agent::{Agent, MessageSink};
use crate::config::FrontendConfig;
use crate::server::Server;

/// A frontend is a Server that can receive Messages from the Agent.
pub trait Frontend: Server + MessageSink {}

/// Create all frontends from their config section.
pub fn create_all(
    configs: &HashMap<String, FrontendConfig>,
    agent: &Arc<Agent>,
    workspace_dir: &Path,
) -> Vec<(String, Arc<dyn Frontend>)> {
    let mut frontends = Vec::new();

    for (name, cfg) in configs {
        match name.as_str() {
            "feishu" => {
                if cfg.app_id.is_empty() || cfg.app_secret.is_empty() {
                    warn!(frontend = name, "skipping: empty app_id or app_secret");
                    continue;
                }
                let f = feishu::Feishu::new(Arc::clone(agent), cfg, workspace_dir);
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
