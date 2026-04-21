use sqlx::{PgPool, Row};

/// Notification persistence (agent → user replies).
pub(crate) struct Notifications {
    pool: PgPool,
}

impl Notifications {
    pub(super) fn new(pool: PgPool) -> Self {
        Self { pool }
    }

    /// Save a notification and link it to the originating message (if any).
    /// Uses a transaction: INSERT notification → UPDATE messages.reply_id.
    /// Returns the notification ID.
    pub(crate) async fn save_notification(
        &self,
        message_id: Option<i64>,
        content: &str,
    ) -> anyhow::Result<i64> {
        let mut tx = self.pool.begin().await?;

        let row = sqlx::query(
            "INSERT INTO notifications (message_id, content) \
             VALUES ($1, $2) \
             RETURNING id",
        )
        .bind(message_id)
        .bind(content)
        .fetch_one(&mut *tx)
        .await?;

        let notif_id: i64 = row.get("id");

        // Link the message to its reply.
        if let Some(msg_id) = message_id {
            sqlx::query("UPDATE messages SET reply_id = $1 WHERE id = $2")
                .bind(notif_id)
                .bind(msg_id)
                .execute(&mut *tx)
                .await?;
        }

        tx.commit().await?;
        Ok(notif_id)
    }
}
