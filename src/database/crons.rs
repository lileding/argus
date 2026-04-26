use chrono::{DateTime, Utc};
use sqlx::{PgPool, Row};

/// A persistent cron job: executes its goal on a schedule.
#[derive(Debug, Clone)]
pub(crate) struct Cron {
    pub(crate) id: i64,
    pub(crate) cron_expr: String,
    pub(crate) goal: String,
    pub(crate) channel: String,
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
        channel: &str,
        msg_id: &str,
    ) -> super::DbResult<i64> {
        let row = sqlx::query(
            "INSERT INTO crons (cron_expr, goal, channel, msg_id) \
             VALUES ($1, $2, $3, $4) RETURNING id",
        )
        .bind(cron_expr)
        .bind(goal)
        .bind(channel)
        .bind(msg_id)
        .fetch_one(&self.pool)
        .await?;
        Ok(row.get("id"))
    }

    /// List all enabled crons (used by Scheduler).
    pub(crate) async fn list_enabled(&self) -> super::DbResult<Vec<Cron>> {
        let rows = sqlx::query(
            "SELECT id, cron_expr, goal, channel, msg_id, last_run_at, created_at \
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
                channel: r.get("channel"),
                msg_id: r.get("msg_id"),
                last_run_at: r.get("last_run_at"),
                created_at: r.get("created_at"),
            })
            .collect())
    }

    /// List enabled crons for a specific channel (used by list_crons tool).
    pub(crate) async fn list_for_channel(&self, channel: &str) -> super::DbResult<Vec<Cron>> {
        let rows = sqlx::query(
            "SELECT id, cron_expr, goal, channel, msg_id, last_run_at, created_at \
             FROM crons WHERE enabled = TRUE AND channel = $1 ORDER BY id",
        )
        .bind(channel)
        .fetch_all(&self.pool)
        .await?;

        Ok(rows
            .iter()
            .map(|r| Cron {
                id: r.get("id"),
                cron_expr: r.get("cron_expr"),
                goal: r.get("goal"),
                channel: r.get("channel"),
                msg_id: r.get("msg_id"),
                last_run_at: r.get("last_run_at"),
                created_at: r.get("created_at"),
            })
            .collect())
    }

    /// Soft-delete (disable) a cron. Returns true if a row was actually disabled.
    /// The channel filter prevents cross-channel cancellation.
    pub(crate) async fn cancel(&self, id: i64, channel: &str) -> super::DbResult<bool> {
        let result = sqlx::query(
            "UPDATE crons SET enabled = FALSE, updated_at = NOW() \
             WHERE id = $1 AND channel = $2 AND enabled = TRUE",
        )
        .bind(id)
        .bind(channel)
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
        channel: &str,
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
             WHERE id = $4 AND channel = $5 AND enabled = TRUE",
        )
        .bind(cron_expr)
        .bind(goal)
        .bind(msg_id)
        .bind(id)
        .bind(channel)
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
