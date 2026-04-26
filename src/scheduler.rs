//! Scheduler: scans persistent crons every minute, fires TaskSpecs.
//!
//! Reuses the Agent's async task path for execution. Each cron firing
//! becomes a TaskSpec with source=Cron, allocated a Task ID from the
//! same shared atomic counter as user-created tasks.

use std::str::FromStr;
use std::sync::atomic::{AtomicU32, Ordering};
use std::time::Duration;

use chrono::Local;
use cron::Schedule;
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;
use tracing::{debug, info, warn};

use crate::agent::{TaskSource, TaskSpec};
use crate::database::Database;
use crate::gateway::Gateway;

const SCAN_INTERVAL: Duration = Duration::from_secs(60);

pub(crate) struct Scheduler<'a> {
    db: &'a Database,
    task_tx: mpsc::Sender<TaskSpec>,
    gateway: &'a Gateway<'a>,
    next_task_id: &'a AtomicU32,
}

impl<'a> Scheduler<'a> {
    pub(crate) fn new(
        db: &'a Database,
        task_tx: mpsc::Sender<TaskSpec>,
        gateway: &'a Gateway<'a>,
        next_task_id: &'a AtomicU32,
    ) -> Self {
        Self {
            db,
            task_tx,
            gateway,
            next_task_id,
        }
    }

    pub(crate) async fn run(&self, cancel: &CancellationToken) {
        info!("scheduler started");
        let mut interval = tokio::time::interval(SCAN_INTERVAL);
        loop {
            tokio::select! {
                _ = interval.tick() => self.scan().await,
                _ = cancel.cancelled() => break,
            }
        }
        info!("scheduler stopped");
    }

    async fn scan(&self) {
        let crons = match self.db.crons.list_enabled().await {
            Ok(c) => c,
            Err(e) => {
                warn!(error = %e, "scheduler: list_enabled failed");
                return;
            }
        };
        if crons.is_empty() {
            return;
        }

        // Cron expressions are interpreted in system local timezone.
        let now = Local::now();
        debug!(count = crons.len(), "scheduler scanning crons");

        for cron in crons {
            let schedule = match Schedule::from_str(&cron.cron_expr) {
                Ok(s) => s,
                Err(e) => {
                    warn!(cron_id = cron.id, expr = cron.cron_expr, error = %e,
                          "invalid cron expression, skipping");
                    continue;
                }
            };

            // Default "last run" = creation time (NOT epoch) so brand-new
            // crons don't fire immediately for past matches.
            let last = cron
                .last_run_at
                .unwrap_or(cron.created_at)
                .with_timezone(&Local);
            let next = match schedule.after(&last).next() {
                Some(n) => n,
                None => {
                    debug!(
                        cron_id = cron.id,
                        expr = cron.cron_expr,
                        "no next firing time"
                    );
                    continue;
                }
            };

            debug!(
                cron_id = cron.id,
                last = %last,
                next = %next,
                now = %now,
                "cron evaluated"
            );

            if next > now {
                continue; // not due yet
            }

            // Due — fire it.
            let port = match self.gateway.outbound_port(&cron.channel) {
                Some(p) => p,
                None => {
                    warn!(
                        cron_id = cron.id,
                        channel = cron.channel,
                        "no outbound port for channel, skipping"
                    );
                    continue;
                }
            };

            let task_id = self.next_task_id.fetch_add(1, Ordering::Relaxed);
            let spec = TaskSpec {
                id: task_id,
                goal: cron.goal.clone(),
                channel: cron.channel.clone(),
                msg_id: cron.msg_id.clone(),
                port,
                source: TaskSource::Cron { cron_id: cron.id },
            };

            info!(cron_id = cron.id, task_id, "cron firing");
            if let Err(e) = self.task_tx.send(spec).await {
                warn!(cron_id = cron.id, error = %e, "failed to submit cron task");
                continue;
            }
            if let Err(e) = self.db.crons.set_last_run_at(cron.id, now.into()).await {
                warn!(cron_id = cron.id, error = %e, "failed to update last_run_at");
            }
        }
    }
}
