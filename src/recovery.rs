//! Crash recovery: replays unreplied messages through the gateway.
//!
//! Runs at startup (immediate scan) and periodically (every 5 minutes).
//! Finds messages with no reply (reply_id IS NULL) from the last 24 hours
//! and sends them to the gateway for reprocessing.

use std::time::Duration;

use tokio_util::sync::CancellationToken;
use tracing::{debug, info, warn};

use crate::database::Database;
use crate::gateway::Gateway;

const SCAN_INTERVAL: Duration = Duration::from_secs(300); // 5 minutes

pub(crate) struct Recovery<'a> {
    db: &'a Database,
    gateway: &'a Gateway<'a>,
}

impl<'a> Recovery<'a> {
    pub(crate) fn new(db: &'a Database, gateway: &'a Gateway<'a>) -> Self {
        Self { db, gateway }
    }

    pub(crate) async fn run(&self, cancel: &CancellationToken) {
        // Immediate scan on startup.
        self.scan().await;

        // Periodic scans.
        loop {
            tokio::select! {
                _ = tokio::time::sleep(SCAN_INTERVAL) => {
                    self.scan().await;
                }
                _ = cancel.cancelled() => break,
            }
        }
    }

    async fn scan(&self) {
        let messages = match self.db.messages.unreplied().await {
            Ok(msgs) => msgs,
            Err(e) => {
                warn!(error = %e, "recovery scan failed");
                return;
            }
        };

        if messages.is_empty() {
            debug!("recovery scan: no unreplied messages");
            return;
        }

        info!(
            count = messages.len(),
            "recovery: replaying unreplied messages"
        );

        for msg in messages {
            let db_msg_id = msg.db_msg_id;
            self.gateway.replay(msg).await;
            debug!(db_msg_id, "recovery: message sent to gateway");
        }
    }
}
