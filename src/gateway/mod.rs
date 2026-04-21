mod feishu;

use std::collections::HashMap;
use std::path::Path;
use std::sync::Arc;
use tracing::{info, warn};

use crate::agent::{Agent, MessageSink};
use crate::config::GatewayImConfig;
use crate::server::Server;

/// An IM adapter that can run (event loop) and receive messages from Agent.
trait Im: Server + MessageSink {}

/// Gateway manages all IM adapters. One gateway to rule them all.
///
/// Implements Server: run() spawns all IMs, stop() stops them all.
/// Main never sees the individual IMs.
pub(crate) struct Gateway {
    ims: Vec<(String, Arc<dyn Im>)>,
}

impl Gateway {
    pub(crate) fn new(
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
}

#[async_trait::async_trait]
impl Server for Gateway {
    /// Spawn all IM adapters and block until they all finish.
    async fn run(self: Arc<Self>) {
        let handles: Vec<_> = self
            .ims
            .iter()
            .map(|(name, im)| {
                let im = Arc::clone(im);
                let name = name.clone();
                tokio::spawn(async move {
                    info!(im = name, "IM adapter started");
                    im.run().await;
                })
            })
            .collect();

        for handle in handles {
            let _ = handle.await;
        }
    }

    /// Stop all IM adapters.
    async fn stop(&self) {
        for (name, im) in &self.ims {
            info!(im = name, "stopping IM adapter");
            im.stop().await;
        }
    }
}
