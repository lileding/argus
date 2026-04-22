use chrono::{DateTime, Utc};
use pgvector::Vector;
use sqlx::{PgPool, Row};

/// Parameters for saving a new inbound message.
pub(crate) struct InboundMessage<'a> {
    pub(crate) channel: &'a str,
    pub(crate) content: &'a str,
    pub(crate) msg_type: &'a str,
    pub(crate) sender_id: &'a str,
    pub(crate) trigger_msg_id: &'a str,
    pub(crate) source_ts: Option<DateTime<Utc>>,
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
    pub(crate) async fn save_received(&self, msg: &InboundMessage<'_>) -> super::DbResult<i64> {
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
    ) -> super::DbResult<()> {
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

    /// Fetch messages that haven't been embedded yet.
    pub(crate) async fn unembedded(&self, limit: i64) -> super::DbResult<Vec<(i64, String)>> {
        let rows = sqlx::query(
            "SELECT id, content FROM messages \
             WHERE embedding IS NULL AND content != '' \
             ORDER BY id DESC LIMIT $1",
        )
        .bind(limit)
        .fetch_all(&self.pool)
        .await?;

        Ok(rows
            .iter()
            .map(|r| (r.get("id"), r.get("content")))
            .collect())
    }

    /// Set the embedding vector for a message.
    pub(crate) async fn set_embedding(&self, id: i64, embedding: &[f32]) -> super::DbResult<()> {
        let vec = Vector::from(embedding.to_vec());
        sqlx::query("UPDATE messages SET embedding = $1 WHERE id = $2 AND embedding IS NULL")
            .bind(vec)
            .bind(id)
            .execute(&self.pool)
            .await?;

        Ok(())
    }

    /// Find messages that were never replied to (for crash recovery).
    /// Only looks at recent messages (last 24h) to avoid replaying ancient history.
    pub(crate) async fn unreplied(&self) -> super::DbResult<Vec<UnrepliedMessage>> {
        let rows = sqlx::query(
            "SELECT id, channel, trigger_msg_id, msg_type, content, ready \
             FROM messages \
             WHERE reply_id IS NULL \
               AND created_at > NOW() - INTERVAL '24 hours' \
             ORDER BY created_at",
        )
        .fetch_all(&self.pool)
        .await?;

        Ok(rows
            .iter()
            .map(|r| UnrepliedMessage {
                db_msg_id: r.get("id"),
                channel: r.get("channel"),
                trigger_msg_id: r
                    .get::<Option<String>, _>("trigger_msg_id")
                    .unwrap_or_default(),
                msg_type: r.get("msg_type"),
                content: r.get("content"),
                ready: r.get("ready"),
            })
            .collect())
    }
}

/// A message that needs recovery (no reply yet).
pub(crate) struct UnrepliedMessage {
    pub(crate) db_msg_id: i64,
    pub(crate) channel: String,
    pub(crate) trigger_msg_id: String,
    pub(crate) msg_type: String,
    pub(crate) content: String,
    pub(crate) ready: bool,
}
