use pgvector::Vector;
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

    /// Fetch notifications that haven't been embedded yet.
    pub(crate) async fn unembedded(&self, limit: i64) -> anyhow::Result<Vec<(i64, String)>> {
        let rows = sqlx::query(
            "SELECT id, content FROM notifications \
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

    /// Set the embedding vector for a notification.
    pub(crate) async fn set_embedding(&self, id: i64, embedding: &[f32]) -> anyhow::Result<()> {
        let vec = Vector::from(embedding.to_vec());
        sqlx::query("UPDATE notifications SET embedding = $1 WHERE id = $2 AND embedding IS NULL")
            .bind(vec)
            .bind(id)
            .execute(&self.pool)
            .await?;

        Ok(())
    }

    /// Fetch long notifications without summaries.
    pub(crate) async fn unsummarized(&self, limit: i64) -> anyhow::Result<Vec<(i64, String)>> {
        let rows = sqlx::query(
            "SELECT id, content FROM notifications \
             WHERE summary IS NULL AND LENGTH(content) > 2400 \
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

    /// Set the summary for a notification.
    pub(crate) async fn set_summary(&self, id: i64, summary: &str) -> anyhow::Result<()> {
        sqlx::query("UPDATE notifications SET summary = $1 WHERE id = $2 AND summary IS NULL")
            .bind(summary)
            .bind(id)
            .execute(&self.pool)
            .await?;

        Ok(())
    }
}
