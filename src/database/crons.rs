use chrono::{DateTime, Utc};
use sqlx::{PgPool, Row};

/// A persistent cron job: executes its goal on a schedule.
#[derive(Debug, Clone)]
pub(crate) struct Cron {
    pub(crate) id: i64,
    pub(crate) cron_expr: String,
    pub(crate) goal: String,
    /// Originating sink (for routing the cron-triggered notification back).
    pub(crate) sink: String,
    /// Channel ID for tenant isolation. None = default channel.
    pub(crate) channel_id: Option<i64>,
    pub(crate) msg_id: String,
    pub(crate) last_run_at: Option<DateTime<Utc>>,
    pub(crate) created_at: DateTime<Utc>,
}

pub(crate) struct Crons {
    pool: PgPool,
}

impl Crons {
    pub(super) fn new(pool: PgPool) -> Self {
        Self { pool }
    }

    /// Insert a new cron, return its ID.
    pub(crate) async fn create(
        &self,
        cron_expr: &str,
        goal: &str,
        sink: &str,
        channel_id: Option<i64>,
        msg_id: &str,
    ) -> super::DbResult<i64> {
        let row = sqlx::query(
            "INSERT INTO crons (cron_expr, goal, sink, channel_id, msg_id) \
             VALUES ($1, $2, $3, $4, $5) RETURNING id",
        )
        .bind(cron_expr)
        .bind(goal)
        .bind(sink)
        .bind(channel_id)
        .bind(msg_id)
        .fetch_one(&self.pool)
        .await?;
        Ok(row.get("id"))
    }

    /// List all enabled crons (used by Scheduler).
    pub(crate) async fn list_enabled(&self) -> super::DbResult<Vec<Cron>> {
        let rows = sqlx::query(
            "SELECT id, cron_expr, goal, sink, channel_id, msg_id, last_run_at, created_at \
             FROM crons WHERE enabled = TRUE",
        )
        .fetch_all(&self.pool)
        .await?;

        Ok(rows
            .iter()
            .map(|r| Cron {
                id: r.get("id"),
                cron_expr: r.get("cron_expr"),
                goal: r.get("goal"),
                sink: r.get("sink"),
                channel_id: r.get("channel_id"),
                msg_id: r.get("msg_id"),
                last_run_at: r.get("last_run_at"),
                created_at: r.get("created_at"),
            })
            .collect())
    }

    /// List enabled crons for a specific channel (used by list_crons tool).
    /// `channel_id = None` lists default-channel crons.
    pub(crate) async fn list_for_channel(
        &self,
        channel_id: Option<i64>,
    ) -> super::DbResult<Vec<Cron>> {
        let rows = sqlx::query(
            "SELECT id, cron_expr, goal, sink, channel_id, msg_id, last_run_at, created_at \
             FROM crons WHERE enabled = TRUE \
               AND channel_id IS NOT DISTINCT FROM $1 \
             ORDER BY id",
        )
        .bind(channel_id)
        .fetch_all(&self.pool)
        .await?;

        Ok(rows
            .iter()
            .map(|r| Cron {
                id: r.get("id"),
                cron_expr: r.get("cron_expr"),
                goal: r.get("goal"),
                sink: r.get("sink"),
                channel_id: r.get("channel_id"),
                msg_id: r.get("msg_id"),
                last_run_at: r.get("last_run_at"),
                created_at: r.get("created_at"),
            })
            .collect())
    }

    /// Soft-delete (disable) a cron. Returns true if a row was actually disabled.
    /// The channel filter prevents cross-channel cancellation.
    pub(crate) async fn cancel(&self, id: i64, channel_id: Option<i64>) -> super::DbResult<bool> {
        let result = sqlx::query(
            "UPDATE crons SET enabled = FALSE, updated_at = NOW() \
             WHERE id = $1 \
               AND channel_id IS NOT DISTINCT FROM $2 \
               AND enabled = TRUE",
        )
        .bind(id)
        .bind(channel_id)
        .execute(&self.pool)
        .await?;
        Ok(result.rows_affected() > 0)
    }

    /// Update fields on a cron. Pass None to leave a field unchanged.
    /// last_run_at is intentionally NOT reset.
    /// Returns true if a row was modified.
    pub(crate) async fn update(
        &self,
        id: i64,
        channel_id: Option<i64>,
        cron_expr: Option<&str>,
        goal: Option<&str>,
        msg_id: &str,
    ) -> super::DbResult<bool> {
        let result = sqlx::query(
            "UPDATE crons SET \
             cron_expr = COALESCE($1, cron_expr), \
             goal = COALESCE($2, goal), \
             msg_id = $3, \
             updated_at = NOW() \
             WHERE id = $4 \
               AND channel_id IS NOT DISTINCT FROM $5 \
               AND enabled = TRUE",
        )
        .bind(cron_expr)
        .bind(goal)
        .bind(msg_id)
        .bind(id)
        .bind(channel_id)
        .execute(&self.pool)
        .await?;
        Ok(result.rows_affected() > 0)
    }

    /// Mark a cron as having just been run.
    pub(crate) async fn set_last_run_at(
        &self,
        id: i64,
        when: DateTime<Utc>,
    ) -> super::DbResult<()> {
        sqlx::query("UPDATE crons SET last_run_at = $1 WHERE id = $2")
            .bind(when)
            .bind(id)
            .execute(&self.pool)
            .await?;
        Ok(())
    }
}
