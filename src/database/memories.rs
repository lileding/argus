use pgvector::Vector;
use sqlx::{PgPool, Row};

/// A pinned memory.
pub(crate) struct Memory {
    pub(crate) category: String,
    pub(crate) content: String,
}

/// Pinned memories persistence.
pub(crate) struct Memories {
    pool: PgPool,
}

impl Memories {
    pub(super) fn new(pool: PgPool) -> Self {
        Self { pool }
    }

    /// List all active memories for a channel (for prompt injection).
    /// `channel_id = None` lists default-channel memories.
    pub(crate) async fn list_active(
        &self,
        channel_id: Option<i64>,
    ) -> super::DbResult<Vec<Memory>> {
        let rows = sqlx::query(
            "SELECT category, content FROM memories \
             WHERE active = TRUE \
               AND channel_id IS NOT DISTINCT FROM $1 \
             ORDER BY created_at",
        )
        .bind(channel_id)
        .fetch_all(&self.pool)
        .await?;

        Ok(rows
            .iter()
            .map(|r| Memory {
                category: r.get("category"),
                content: r.get("content"),
            })
            .collect())
    }

    /// Fetch memories without embeddings.
    pub(crate) async fn unembedded(&self, limit: i64) -> super::DbResult<Vec<(i64, String)>> {
        let rows = sqlx::query(
            "SELECT id, content FROM memories \
             WHERE embedding IS NULL AND active = TRUE \
             ORDER BY id LIMIT $1",
        )
        .bind(limit)
        .fetch_all(&self.pool)
        .await?;

        Ok(rows
            .iter()
            .map(|r| (r.get("id"), r.get("content")))
            .collect())
    }

    /// Save a new pinned memory. Returns the memory ID.
    pub(crate) async fn save(
        &self,
        category: &str,
        content: &str,
        channel_id: Option<i64>,
    ) -> super::DbResult<i64> {
        let row = sqlx::query(
            "INSERT INTO memories (category, content, channel_id, active) \
             VALUES ($1, $2, $3, TRUE) RETURNING id",
        )
        .bind(category)
        .bind(content)
        .bind(channel_id)
        .fetch_one(&self.pool)
        .await?;
        Ok(row.get("id"))
    }

    /// Deactivate a memory by ID, scoped to the given channel.
    /// Returns true if a memory was actually deactivated.
    pub(crate) async fn deactivate(
        &self,
        id: i64,
        channel_id: Option<i64>,
    ) -> super::DbResult<bool> {
        let result = sqlx::query(
            "UPDATE memories SET active = FALSE \
             WHERE id = $1 \
               AND channel_id IS NOT DISTINCT FROM $2 \
               AND active = TRUE",
        )
        .bind(id)
        .bind(channel_id)
        .execute(&self.pool)
        .await?;
        Ok(result.rows_affected() > 0)
    }

    /// Set embedding for a memory.
    pub(crate) async fn set_embedding(&self, id: i64, embedding: &[f32]) -> super::DbResult<()> {
        let vec = Vector::from(embedding.to_vec());
        sqlx::query("UPDATE memories SET embedding = $1 WHERE id = $2 AND embedding IS NULL")
            .bind(vec)
            .bind(id)
            .execute(&self.pool)
            .await?;
        Ok(())
    }
}
