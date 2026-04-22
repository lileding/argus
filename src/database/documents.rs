use pgvector::Vector;
use sqlx::{PgPool, Row};

/// Document metadata row.
pub(crate) struct PendingDocument {
    pub id: i64,
    pub filename: String,
    pub file_path: String,
}

/// Documents + chunks persistence for RAG.
pub(crate) struct Documents {
    pool: PgPool,
}

impl Documents {
    pub(super) fn new(pool: PgPool) -> Self {
        Self { pool }
    }

    /// Save a new document as pending. Returns the document ID.
    pub(crate) async fn save_document(
        &self,
        filename: &str,
        file_path: &str,
    ) -> anyhow::Result<i64> {
        let row = sqlx::query(
            "INSERT INTO documents (filename, file_path, status) \
             VALUES ($1, $2, 'pending') \
             RETURNING id",
        )
        .bind(filename)
        .bind(file_path)
        .fetch_one(&self.pool)
        .await?;
        Ok(row.get("id"))
    }

    /// Update document status.
    pub(crate) async fn update_status(
        &self,
        id: i64,
        status: &str,
        error_msg: &str,
    ) -> anyhow::Result<()> {
        sqlx::query("UPDATE documents SET status = $1, error_msg = $2 WHERE id = $3")
            .bind(status)
            .bind(error_msg)
            .bind(id)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    /// Fetch pending documents for background processing.
    pub(crate) async fn pending(&self, limit: i64) -> anyhow::Result<Vec<PendingDocument>> {
        let rows = sqlx::query(
            "SELECT id, filename, file_path FROM documents \
             WHERE status = 'pending' ORDER BY id LIMIT $1",
        )
        .bind(limit)
        .fetch_all(&self.pool)
        .await?;

        Ok(rows
            .iter()
            .map(|r| PendingDocument {
                id: r.get("id"),
                filename: r.get("filename"),
                file_path: r.get("file_path"),
            })
            .collect())
    }

    /// Save text chunks for a document.
    pub(crate) async fn save_chunks(&self, doc_id: i64, chunks: &[String]) -> anyhow::Result<()> {
        for (i, content) in chunks.iter().enumerate() {
            sqlx::query(
                "INSERT INTO chunks (document_id, chunk_index, content) \
                 VALUES ($1, $2, $3)",
            )
            .bind(doc_id)
            .bind(i as i32)
            .bind(content)
            .execute(&self.pool)
            .await?;
        }
        Ok(())
    }

    /// Fetch chunks without embeddings.
    pub(crate) async fn unembedded_chunks(&self, limit: i64) -> anyhow::Result<Vec<(i64, String)>> {
        let rows = sqlx::query(
            "SELECT id, content FROM chunks \
             WHERE embedding IS NULL ORDER BY id LIMIT $1",
        )
        .bind(limit)
        .fetch_all(&self.pool)
        .await?;

        Ok(rows
            .iter()
            .map(|r| (r.get("id"), r.get("content")))
            .collect())
    }

    /// Set embedding on a chunk.
    pub(crate) async fn set_chunk_embedding(
        &self,
        id: i64,
        embedding: &[f32],
    ) -> anyhow::Result<()> {
        let vec = Vector::from(embedding.to_vec());
        sqlx::query("UPDATE chunks SET embedding = $1 WHERE id = $2 AND embedding IS NULL")
            .bind(vec)
            .bind(id)
            .execute(&self.pool)
            .await?;
        Ok(())
    }
}
