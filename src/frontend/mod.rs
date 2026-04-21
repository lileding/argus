mod feishu;

use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use tracing::{info, warn};

use crate::agent::Agent;
use crate::config::Config;

/// A frontend can be run (blocking event loop) and stopped (graceful shutdown).
/// Uses Pin<Box<...>> for dyn compatibility.
pub trait Frontend: Send + Sync {
    fn run(self: Arc<Self>) -> Pin<Box<dyn Future<Output = ()> + Send>>;
    fn stop(&self) -> Pin<Box<dyn Future<Output = ()> + Send + '_>>;
}

/// Create all frontends defined in config. Returns (name, handle) pairs.
/// Main doesn't need to know about specific frontend types.
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
