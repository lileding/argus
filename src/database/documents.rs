use pgvector::Vector;
use sqlx::{PgPool, Row};

/// Document metadata row.
pub(crate) struct PendingDocument {
    pub(crate) id: i64,
    pub(crate) filename: String,
    pub(crate) file_path: String,
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
    ) -> super::DbResult<i64> {
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

    /// Update document status with state machine enforcement.
    pub(crate) async fn update_status(
        &self,
        id: i64,
        status: &str,
        error_msg: &str,
    ) -> super::DbResult<()> {
        let valid_from: &[&str] = match status {
            "processing" => &["pending"],
            "ready" => &["processing"],
            "error" => &["pending", "processing"],
            _ => {
                return Err(super::DatabaseError::InvalidState(format!(
                    "invalid document status: {status}"
                )));
            }
        };
        let result = sqlx::query(
            "UPDATE documents SET status = $1, error_msg = $2 \
             WHERE id = $3 AND status = ANY($4)",
        )
        .bind(status)
        .bind(error_msg)
        .bind(id)
        .bind(valid_from)
        .execute(&self.pool)
        .await?;

        if result.rows_affected() == 0 {
            return Err(super::DatabaseError::InvalidState(format!(
                "document {id}: stale status transition to '{status}'"
            )));
        }
        Ok(())
    }

    /// Fetch pending documents for background processing.
    pub(crate) async fn pending(&self, limit: i64) -> super::DbResult<Vec<PendingDocument>> {
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

    /// Save text chunks for a document (transactional).
    pub(crate) async fn save_chunks(&self, doc_id: i64, chunks: &[String]) -> super::DbResult<()> {
        let mut tx = self.pool.begin().await?;
        for (i, content) in chunks.iter().enumerate() {
            sqlx::query(
                "INSERT INTO chunks (document_id, chunk_index, content) \
                 VALUES ($1, $2, $3)",
            )
            .bind(doc_id)
            .bind(i as i32)
            .bind(content)
            .execute(&mut *tx)
            .await?;
        }
        tx.commit().await?;
        Ok(())
    }

    /// Fetch chunks without embeddings.
    pub(crate) async fn unembedded_chunks(
        &self,
        limit: i64,
    ) -> super::DbResult<Vec<(i64, String)>> {
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
    ) -> super::DbResult<()> {
        let vec = Vector::from(embedding.to_vec());
        sqlx::query("UPDATE chunks SET embedding = $1 WHERE id = $2 AND embedding IS NULL")
            .bind(vec)
            .bind(id)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    /// Semantic search over document chunks. Returns (filename, chunk_content, similarity).
    pub(crate) async fn search_chunks(
        &self,
        embedding: &[f32],
        limit: i64,
        filename_filter: Option<&str>,
    ) -> super::DbResult<Vec<(String, i32, String, f64)>> {
        let vec = pgvector::Vector::from(embedding.to_vec());
        let rows = if let Some(fname) = filename_filter {
            sqlx::query(
                "SELECT d.filename, c.chunk_index, c.content, \
                        1 - (c.embedding <=> $1) AS similarity \
                 FROM chunks c JOIN documents d ON d.id = c.document_id \
                 WHERE c.embedding IS NOT NULL AND d.filename ILIKE '%' || $2 || '%' \
                 ORDER BY c.embedding <=> $1 LIMIT $3",
            )
            .bind(&vec)
            .bind(fname)
            .bind(limit)
            .fetch_all(&self.pool)
            .await?
        } else {
            sqlx::query(
                "SELECT d.filename, c.chunk_index, c.content, \
                        1 - (c.embedding <=> $1) AS similarity \
                 FROM chunks c JOIN documents d ON d.id = c.document_id \
                 WHERE c.embedding IS NOT NULL \
                 ORDER BY c.embedding <=> $1 LIMIT $2",
            )
            .bind(&vec)
            .bind(limit)
            .fetch_all(&self.pool)
            .await?
        };

        Ok(rows
            .iter()
            .map(|r| {
                (
                    r.get::<String, _>("filename"),
                    r.get::<i32, _>("chunk_index"),
                    r.get::<String, _>("content"),
                    r.get::<f64, _>("similarity"),
                )
            })
            .collect())
    }

    /// List all documents with their status.
    pub(crate) async fn list_all(
        &self,
    ) -> super::DbResult<Vec<(i64, String, String, i64, chrono::DateTime<chrono::Utc>)>> {
        let rows = sqlx::query(
            "SELECT d.id, d.filename, d.status, d.created_at, \
                    COUNT(c.id) AS chunk_count \
             FROM documents d LEFT JOIN chunks c ON c.document_id = d.id \
             GROUP BY d.id ORDER BY d.created_at DESC",
        )
        .fetch_all(&self.pool)
        .await?;

        Ok(rows
            .iter()
            .map(|r| {
                (
                    r.get::<i64, _>("id"),
                    r.get::<String, _>("filename"),
                    r.get::<String, _>("status"),
                    r.get::<i64, _>("chunk_count"),
                    r.get::<chrono::DateTime<chrono::Utc>, _>("created_at"),
                )
            })
            .collect())
    }
}
