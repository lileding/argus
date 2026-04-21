use chrono::{DateTime, Utc};
use sqlx::{PgPool, Row};

/// Parameters for saving a new inbound message.
pub(crate) struct InboundMessage<'a> {
    pub channel: &'a str,
    pub content: &'a str,
    pub msg_type: &'a str,
    pub sender_id: &'a str,
    pub trigger_msg_id: &'a str,
    pub source_ts: Option<DateTime<Utc>>,
}

/// Message persistence. Two states: ready=false (received) → ready=true (processed).
pub(crate) struct Messages {
    pool: PgPool,
}

impl Messages {
    pub(super) fn new(pool: PgPool) -> Self {
        Self { pool }
    }

    /// Save a new inbound user message with ready=false.
    /// Returns the database-assigned message ID.
    pub(crate) async fn save_received(&self, msg: &InboundMessage<'_>) -> anyhow::Result<i64> {
        let row = sqlx::query(
            "INSERT INTO messages \
                (channel, content, msg_type, sender_id, trigger_msg_id, source_ts, ready) \
             VALUES ($1, $2, $3, $4, $5, $6, FALSE) \
             RETURNING id",
        )
        .bind(msg.channel)
        .bind(msg.content)
        .bind(msg.msg_type)
        .bind(msg.sender_id)
        .bind(msg.trigger_msg_id)
        .bind(msg.source_ts)
        .fetch_one(&self.pool)
        .await?;

        Ok(row.get("id"))
    }

    /// Update a message after media processing: set processed content,
    /// file paths, and mark as ready.
    pub(crate) async fn save_ready(
        &self,
        msg_id: i64,
        content: &str,
        file_paths: &[String],
    ) -> anyhow::Result<()> {
        sqlx::query(
            "UPDATE messages \
             SET content = $1, file_paths = $2, ready = TRUE \
             WHERE id = $3 AND NOT ready",
        )
        .bind(content)
        .bind(file_paths)
        .bind(msg_id)
        .execute(&self.pool)
        .await?;

        Ok(())
    }
}
