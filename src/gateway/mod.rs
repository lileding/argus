mod feishu;
mod transcribe;

use std::collections::HashMap;
use std::path::Path;
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;
use tracing::{info, warn};

use crate::agent::Task;
use crate::config::GatewayImConfig;
use crate::database::{Database, messages::UnrepliedMessage};
use crate::upstream::Upstream;

/// An IM adapter: long-running event loop.
#[async_trait::async_trait]
trait Im: Send + Sync {
    async fn run(&self, cancel: &CancellationToken);
}

/// Gateway manages all IM adapters.
pub(crate) struct Gateway<'a> {
    ims: Vec<(String, Box<dyn Im + 'a>)>,
    /// Recovery channels: IM name prefix → sender.
    recover_txs: HashMap<String, mpsc::Sender<UnrepliedMessage>>,
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
        let mut recover_txs: HashMap<String, mpsc::Sender<UnrepliedMessage>> = HashMap::new();

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

                    let (recover_tx, recover_rx) = mpsc::channel(64);
                    let f = feishu::Feishu::new(
                        port.clone(),
                        db,
                        cfg,
                        workspace_dir,
                        transcriber,
                        recover_rx,
                    );
                    info!(im = name, "IM adapter created");
                    ims.push((name.clone(), Box::new(f)));
                    recover_txs.insert(name.clone(), recover_tx);
                }
                other => {
                    warn!(im = other, "unknown IM type, skipping");
                }
            }
        }

        info!(count = ims.len(), "gateway created");
        Self { ims, recover_txs }
    }

    /// Replay an unreplied message through the appropriate IM adapter.
    pub(crate) async fn replay(&self, msg: UnrepliedMessage) {
        // Route by channel prefix: "feishu:..." → feishu IM.
        let im_name = if msg.channel.starts_with("feishu:") {
            "feishu"
        } else {
            warn!(channel = msg.channel, "unknown IM for recovery, skipping");
            return;
        };

        if let Some(tx) = self.recover_txs.get(im_name) {
            if let Err(e) = tx.send(msg).await {
                warn!(im = im_name, error = %e, "failed to send recovery message");
            }
        } else {
            warn!(im = im_name, "no IM adapter for recovery");
        }
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
