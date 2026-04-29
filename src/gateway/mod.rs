mod feishu;
mod transcribe;

use std::collections::HashMap;
use std::path::Path;
use tokio::sync::{Mutex, mpsc};
use tokio_util::sync::CancellationToken;
use tracing::{debug, info, warn};

use crate::agent::{Message, Notification};
use crate::config::GatewayImConfig;
use crate::database::{Database, messages::UnrepliedMessage};
use crate::upstream::Upstream;

const OUTBOUND_QUEUE: usize = 64;

/// An IM adapter: long-running event loop. Owns its own internal notification
/// queue (the Gateway dispatcher pushes into `notification_tx`).
#[async_trait::async_trait]
trait Im: Send + Sync {
    async fn run(&self, cancel: &CancellationToken);
    /// Sender used by the Gateway dispatcher to forward notifications to this IM.
    fn notification_tx(&self) -> &mpsc::Sender<Notification>;
}

/// Gateway manages all IM adapters. Owns a single outbound MPSC; an internal
/// dispatcher reads from it and routes each Notification by `sink` prefix
/// (e.g. `feishu:...`) to the matching IM's internal queue.
pub(crate) struct Gateway<'a> {
    ims: Vec<(String, Box<dyn Im + 'a>)>,
    /// Recovery channels: IM name prefix → sender.
    recover_txs: HashMap<String, mpsc::Sender<UnrepliedMessage>>,
    /// Single outbound entry point shared by Agent (via Message.port / TaskSpec.port)
    /// and Scheduler. Cloned freely; routing happens inside the dispatcher.
    outbound_tx: mpsc::Sender<Notification>,
    outbound_rx: Mutex<mpsc::Receiver<Notification>>,
}

impl<'a> Gateway<'a> {
    pub(crate) fn new(
        configs: &HashMap<String, GatewayImConfig>,
        port: mpsc::Sender<Message>,
        upstream_reg: &Upstream,
        db: &'a Database,
        workspace_dir: &Path,
    ) -> Self {
        let (outbound_tx, outbound_rx) = mpsc::channel(OUTBOUND_QUEUE);

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
                        outbound_tx.clone(),
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
        Self {
            ims,
            recover_txs,
            outbound_tx,
            outbound_rx: Mutex::new(outbound_rx),
        }
    }

    /// Replay an unreplied message through the appropriate IM adapter.
    pub(crate) async fn replay(&self, msg: UnrepliedMessage) {
        // Route by sink prefix: "feishu:..." → feishu IM.
        let im_name = match im_name_for_sink(&msg.sink) {
            Some(n) => n,
            None => {
                warn!(sink = msg.sink, "unknown IM for recovery, skipping");
                return;
            }
        };

        if let Some(tx) = self.recover_txs.get(im_name) {
            if let Err(e) = tx.send(msg).await {
                warn!(im = im_name, error = %e, "failed to send recovery message");
            }
        } else {
            warn!(im = im_name, "no IM adapter for recovery");
        }
    }

    /// The single outbound port. Cloned freely by callers (Scheduler, Agent
    /// handing it off via Message.port / TaskSpec.port). The Gateway internally
    /// dispatches by `notification.sink`.
    pub(crate) fn outbound_port(&self) -> mpsc::Sender<Notification> {
        self.outbound_tx.clone()
    }

    pub(crate) async fn run(&self, cancel: &CancellationToken) {
        let dispatcher = self.dispatch_outbound(cancel);
        let ims = futures::future::join_all(self.ims.iter().map(|(name, im)| async move {
            info!(im = name.as_str(), "IM adapter started");
            im.run(cancel).await;
            info!(im = name.as_str(), "IM adapter stopped");
        }));
        tokio::join!(dispatcher, ims);
    }

    /// Read from the single outbound mpsc and forward each Notification to the
    /// IM determined by its sink prefix. Unknown sinks are warned and dropped.
    async fn dispatch_outbound(&self, cancel: &CancellationToken) {
        let mut rx = self.outbound_rx.lock().await;
        loop {
            tokio::select! {
                notif = rx.recv() => {
                    let Some(notif) = notif else {
                        debug!("gateway outbound channel closed");
                        break;
                    };
                    let im_name = match im_name_for_sink(&notif.sink) {
                        Some(n) => n,
                        None => {
                            warn!(sink = notif.sink, msg_id = notif.msg_id,
                                  "unknown sink prefix, dropping notification");
                            continue;
                        }
                    };
                    let im = self.ims.iter().find(|(n, _)| n == im_name).map(|(_, im)| im);
                    let Some(im) = im else {
                        warn!(im = im_name, sink = notif.sink, msg_id = notif.msg_id,
                              "no IM adapter, dropping notification");
                        continue;
                    };
                    if let Err(e) = im.notification_tx().send(notif).await {
                        warn!(im = im_name, error = %e, "forward to IM failed");
                    }
                }
                _ = cancel.cancelled() => {
                    info!("gateway dispatcher shutting down, draining outbound");
                    while let Ok(Some(notif)) = tokio::time::timeout(
                        std::time::Duration::from_millis(500), rx.recv()).await
                    {
                        let im_name = match im_name_for_sink(&notif.sink) {
                            Some(n) => n,
                            None => continue,
                        };
                        if let Some((_, im)) = self.ims.iter().find(|(n, _)| n == im_name) {
                            let _ = im.notification_tx().send(notif).await;
                        }
                    }
                    break;
                }
            }
        }
    }
}

/// Map a sink string to its IM adapter name by prefix.
/// Sinks look like `feishu:p2p:ou_xxx`, `slack:channel:Cxxx`, etc.
fn im_name_for_sink(sink: &str) -> Option<&'static str> {
    if sink.starts_with("feishu:") {
        Some("feishu")
    } else {
        None
    }
}
