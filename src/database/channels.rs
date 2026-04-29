//! Channels table — the tenant unit of the channel system. Each row maps a
//! human-readable name (declared in TOML) to a channel_id used by other
//! tables. The `sinks` column carries the TOML-declared sinks for record
//! purposes; runtime routing reads the live in-memory map populated at
//! startup.

use sqlx::{PgPool, Row};

pub(crate) struct Channels {
    pool: PgPool,
}

impl Channels {
    pub(super) fn new(pool: PgPool) -> Self {
        Self { pool }
    }

    /// Insert or update a channel by name; persists `sinks` as JSONB and
    /// returns the channel_id.
    pub(crate) async fn upsert(&self, name: &str, sinks: &[String]) -> super::DbResult<i64> {
        let sinks_json = serde_json::to_value(sinks).unwrap_or_else(|_| serde_json::json!([]));
        let row = sqlx::query(
            "INSERT INTO channels (name, sinks) VALUES ($1, $2) \
             ON CONFLICT (name) DO UPDATE SET sinks = EXCLUDED.sinks \
             RETURNING id",
        )
        .bind(name)
        .bind(sinks_json)
        .fetch_one(&self.pool)
        .await?;
        Ok(row.get("id"))
    }

    /// List all channel rows (id, name) for startup reconciliation against
    /// the TOML config.
    pub(crate) async fn list_all(&self) -> super::DbResult<Vec<(i64, String)>> {
        let rows = sqlx::query("SELECT id, name FROM channels ORDER BY id")
            .fetch_all(&self.pool)
            .await?;
        Ok(rows.iter().map(|r| (r.get("id"), r.get("name"))).collect())
    }
}
