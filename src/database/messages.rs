use chrono::{DateTime, Utc};
use sqlx::{PgPool, Row};

/// Stored message row from the `messages` table.
#[derive(Debug, Clone)]
#[allow(dead_code)]
pub(crate) struct StoredMessage {
    pub id: i64,
    pub chat_id: String,
    pub role: String,
    pub content: String,
    pub source_im: String,
    pub channel: String,
    pub msg_type: String,
    pub file_paths: Vec<String>,
    pub sender_id: Option<String>,
    pub reply_status: Option<String>,
    pub trigger_msg_id: Option<String>,
    pub created_at: DateTime<Utc>,
}

/// Message persistence. Three-state machine: received → ready → replied.
pub(crate) struct Messages {
    pool: PgPool,
}

impl Messages {
    pub(super) fn new(pool: PgPool) -> Self {
        Self { pool }
    }

    /// Save a new inbound user message with status=received.
    /// Returns the database-assigned message ID.
    pub(crate) async fn save_received(
        &self,
        chat_id: &str,
        content: &str,
        source_im: &str,
        msg_type: &str,
        sender_id: &str,
        trigger_msg_id: &str,
    ) -> anyhow::Result<i64> {
        let row = sqlx::query(
            "INSERT INTO messages \
                (chat_id, role, content, source_im, channel, msg_type, sender_id, \
                 reply_status, trigger_msg_id) \
             VALUES ($1, 'user', $2, $3, $4, $5, $6, 'received', $7) \
             RETURNING id",
        )
        .bind(chat_id)
        .bind(content)
        .bind(source_im)
        .bind(chat_id) // channel = chat_id for now
        .bind(msg_type)
        .bind(sender_id)
        .bind(trigger_msg_id)
        .fetch_one(&self.pool)
        .await?;

        Ok(row.get("id"))
    }

    /// Update a message after media processing: set processed content,
    /// file paths, and transition status received → ready.
    pub(crate) async fn save_ready(
        &self,
        msg_id: i64,
        content: &str,
        file_paths: &[String],
    ) -> anyhow::Result<()> {
        sqlx::query(
            "UPDATE messages \
             SET content = $1, file_paths = $2, reply_status = 'ready' \
             WHERE id = $3 AND reply_status = 'received'",
        )
        .bind(content)
        .bind(file_paths)
        .bind(msg_id)
        .execute(&self.pool)
        .await?;

        Ok(())
    }

    /// Mark a message as replied (terminal state).
    /// Guards against reverting from terminal state.
    pub(crate) async fn save_replied(&self, msg_id: i64) -> anyhow::Result<()> {
        sqlx::query(
            "UPDATE messages \
             SET reply_status = 'replied' \
             WHERE id = $1 AND reply_status != 'replied'",
        )
        .bind(msg_id)
        .execute(&self.pool)
        .await?;

        Ok(())
    }
}
