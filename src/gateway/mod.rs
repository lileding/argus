mod feishu;

use std::collections::HashMap;
use std::path::Path;
use std::sync::Arc;
use tracing::{info, warn};

use crate::agent::{Agent, MessageSink};
use crate::config::GatewayImConfig;
use crate::server::Server;

/// An IM adapter that can run (event loop) and receive messages from Agent.
pub trait Im: Server + MessageSink {}

/// Gateway manages all IM adapters. One gateway to rule them all.
///
/// Hierarchy: Gateway → IM (feishu/slack/...) → Channel (per-chat, future)
pub struct Gateway {
    ims: Vec<(String, Arc<dyn Im>)>,
}

impl Gateway {
    /// Create the gateway with all configured IM adapters.
    pub fn new(
        configs: &HashMap<String, GatewayImConfig>,
        agent: &Arc<Agent>,
        workspace_dir: &Path,
    ) -> Self {
        let mut ims = Vec::new();

        for (name, cfg) in configs {
            match name.as_str() {
                "feishu" => {
                    if cfg.app_id.is_empty() || cfg.app_secret.is_empty() {
                        warn!(im = name, "skipping: empty app_id or app_secret");
                        continue;
                    }
                    let f = feishu::Feishu::new(Arc::clone(agent), cfg, workspace_dir);
                    info!(im = name, "IM adapter created");
                    ims.push((name.clone(), f as Arc<dyn Im>));
                }
                other => {
                    warn!(im = other, "unknown IM type, skipping");
                }
            }
        }

        info!(count = ims.len(), "gateway created");
        Self { ims }
    }

    /// Spawn all IM adapters. Returns join handles.
    pub fn spawn_all(&self) -> Vec<tokio::task::JoinHandle<()>> {
        self.ims
            .iter()
            .map(|(name, im)| {
                let im = Arc::clone(im);
                let name = name.clone();
                tokio::spawn(async move {
                    info!(im = name, "IM adapter started");
                    im.run().await;
                })
            })
            .collect()
    }

    /// Stop all IM adapters.
    pub async fn stop(&self) {
        for (name, im) in &self.ims {
            info!(im = name, "stopping IM adapter");
            im.stop().await;
        }
    }
}
