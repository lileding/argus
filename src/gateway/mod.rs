mod feishu;
mod transcribe;

use std::collections::HashMap;
use std::path::Path;
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;
use tracing::{info, warn};

use crate::agent::Task;
use crate::config::GatewayImConfig;
use crate::database::Database;
use crate::upstream::Upstream;

/// An IM adapter: long-running event loop.
#[async_trait::async_trait]
trait Im: Send + Sync {
    async fn run(&self, cancel: &CancellationToken);
}

/// Gateway manages all IM adapters.
pub(crate) struct Gateway<'a> {
    ims: Vec<(String, Box<dyn Im + 'a>)>,
}

impl<'a> Gateway<'a> {
    pub(crate) fn new(
        configs: &HashMap<String, GatewayImConfig>,
        port: mpsc::Sender<Task>,
        upstream_reg: &Upstream,
        db: &'a Database,
        workspace_dir: &Path,
    ) -> Self {
        let mut ims: Vec<(String, Box<dyn Im + 'a>)> = Vec::new();

        for (name, cfg) in configs {
            match name.as_str() {
                "feishu" => {
                    if cfg.app_id.is_empty() || cfg.app_secret.is_empty() {
                        warn!(im = name, "skipping: empty app_id or app_secret");
                        continue;
                    }
                    // Build transcription client if configured.
                    let transcriber = if !cfg.transcription.upstream.is_empty() {
                        match upstream_reg.get_config(&cfg.transcription.upstream) {
                            Some(up_cfg) => {
                                let base_url = up_cfg.effective_base_url();
                                Some(transcribe::TranscribeClient::new(
                                    base_url,
                                    &up_cfg.api_key,
                                    &cfg.transcription.model_name,
                                ))
                            }
                            None => {
                                warn!(
                                    upstream = cfg.transcription.upstream,
                                    "transcription upstream not found, skipping"
                                );
                                None
                            }
                        }
                    } else {
                        None
                    };

                    let f = feishu::Feishu::new(port.clone(), db, cfg, workspace_dir, transcriber);
                    info!(im = name, "IM adapter created");
                    ims.push((name.clone(), Box::new(f)));
                }
                other => {
                    warn!(im = other, "unknown IM type, skipping");
                }
            }
        }

        info!(count = ims.len(), "gateway created");
        Self { ims }
    }

    pub(crate) async fn run(&self, cancel: &CancellationToken) {
        futures::future::join_all(self.ims.iter().map(|(name, im)| async {
            info!(im = name.as_str(), "IM adapter started");
            im.run(cancel).await;
            info!(im = name.as_str(), "IM adapter stopped");
        }))
        .await;
    }
}
