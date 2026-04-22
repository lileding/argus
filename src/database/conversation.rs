use pgvector::Vector;
use sqlx::{PgPool, Row};

use crate::upstream::types as model;

/// Max reply chars before using summary instead.
const SUMMARY_THRESHOLD: usize = 800;

/// A row from the conversation view.
struct ConversationRow {
    id: i64,
    user_content: String,
    reply_content: Option<String>,
    reply_summary: Option<String>,
}

/// Queries the `conversation` view to build model-ready context.
pub(crate) struct Conversation {
    pool: PgPool,
}

impl Conversation {
    pub(super) fn new(pool: PgPool) -> Self {
        Self { pool }
    }

    /// Load the most recent conversation turns for a channel.
    /// Excludes a specific message ID (the current one being processed).
    /// Returns (row_id, user_msg, optional_reply_msg) in chronological order.
    pub(crate) async fn recent(
        &self,
        channel: &str,
        exclude_id: Option<i64>,
        limit: i64,
    ) -> super::DbResult<Vec<(i64, model::Message, Option<model::Message>)>> {
        let rows = sqlx::query(
            "SELECT id, user_content, reply_content, reply_summary \
             FROM conversation \
             WHERE channel = $1 AND ($2::bigint IS NULL OR id != $2) \
             ORDER BY user_ts DESC \
             LIMIT $3",
        )
        .bind(channel)
        .bind(exclude_id)
        .bind(limit)
        .fetch_all(&self.pool)
        .await?;

        // Rows come newest-first; reverse to chronological.
        let mut results = Vec::new();
        for row in rows.iter().rev() {
            let r = ConversationRow {
                id: row.get("id"),
                user_content: row.get("user_content"),
                reply_content: row.get("reply_content"),
                reply_summary: row.get("reply_summary"),
            };
            let reply = format_reply(&r).map(model::Message::assistant);
            results.push((r.id, model::Message::user(&r.user_content), reply));
        }

        Ok(results)
    }

    /// Semantic search: find messages similar to the given embedding vector.
    /// Excludes a specific message ID (the current one being processed).
    pub(crate) async fn search(
        &self,
        embedding: &[f32],
        channel: &str,
        exclude_id: Option<i64>,
        limit: i64,
    ) -> super::DbResult<Vec<(i64, f64, model::Message, Option<model::Message>)>> {
        let vec = Vector::from(embedding.to_vec());
        let rows = sqlx::query(
            "SELECT id, user_content, reply_content, reply_summary, \
                    1 - (user_embedding <=> $1) AS similarity \
             FROM conversation \
             WHERE channel = $2 AND user_embedding IS NOT NULL \
               AND ($3::bigint IS NULL OR id != $3) \
             ORDER BY user_embedding <=> $1 \
             LIMIT $4",
        )
        .bind(vec)
        .bind(channel)
        .bind(exclude_id)
        .bind(limit)
        .fetch_all(&self.pool)
        .await?;

        let mut results = Vec::new();
        for row in &rows {
            let r = ConversationRow {
                id: row.get("id"),
                user_content: row.get("user_content"),
                reply_content: row.get("reply_content"),
                reply_summary: row.get("reply_summary"),
            };
            let similarity: f64 = row.get("similarity");
            let reply = format_reply(&r).map(model::Message::assistant);
            results.push((
                r.id,
                similarity,
                model::Message::user(&r.user_content),
                reply,
            ));
        }

        Ok(results)
    }

    /// Semantic search with timestamps — for the search_history tool.
    /// Returns (similarity, timestamp, user_content, reply_content).
    pub(crate) async fn search_with_time(
        &self,
        embedding: &[f32],
        channel: &str,
        limit: i64,
    ) -> super::DbResult<Vec<(f64, chrono::DateTime<chrono::Utc>, String, Option<String>)>> {
        let vec = Vector::from(embedding.to_vec());
        let rows = sqlx::query(
            "SELECT user_content, reply_content, reply_summary, user_ts, \
                    1 - (user_embedding <=> $1) AS similarity \
             FROM conversation \
             WHERE channel = $2 AND user_embedding IS NOT NULL \
             ORDER BY user_embedding <=> $1 \
             LIMIT $3",
        )
        .bind(vec)
        .bind(channel)
        .bind(limit)
        .fetch_all(&self.pool)
        .await?;

        Ok(rows
            .iter()
            .map(|row| {
                let similarity: f64 = row.get("similarity");
                let ts: chrono::DateTime<chrono::Utc> = row.get("user_ts");
                let user: String = row.get("user_content");
                let reply: Option<String> = row.get("reply_content");
                let summary: Option<String> = row.get("reply_summary");
                // Use summary for long replies (same as format_reply).
                let reply_text = reply.map(|r| {
                    if r.chars().count() > SUMMARY_THRESHOLD {
                        if let Some(s) = summary
                            && !s.is_empty()
                        {
                            return format!("[Summary] {s}");
                        }
                        let t: String = r.chars().take(SUMMARY_THRESHOLD).collect();
                        return format!("{t} …[truncated]");
                    }
                    r
                });
                (similarity, ts, user, reply_text)
            })
            .collect())
    }

    /// Search notifications (agent replies) by embedding similarity.
    /// Returns assistant messages matching the query, with summary truncation.
    pub(crate) async fn search_replies(
        &self,
        embedding: &[f32],
        limit: i64,
    ) -> super::DbResult<Vec<(f64, model::Message)>> {
        let vec = Vector::from(embedding.to_vec());
        let rows = sqlx::query(
            "SELECT content, summary, \
                    1 - (embedding <=> $1) AS similarity \
             FROM notifications \
             WHERE embedding IS NOT NULL \
             ORDER BY embedding <=> $1 \
             LIMIT $2",
        )
        .bind(vec)
        .bind(limit)
        .fetch_all(&self.pool)
        .await?;

        let mut results = Vec::new();
        for row in &rows {
            let similarity: f64 = row.get("similarity");
            let content: String = row.get("content");
            let summary: Option<String> = row.get("summary");

            // Apply same summary truncation as conversation replies.
            let text = if content.chars().count() > SUMMARY_THRESHOLD {
                if let Some(ref s) = summary
                    && !s.is_empty()
                {
                    format!("[Summary] {s}")
                } else {
                    let truncated: String = content.chars().take(SUMMARY_THRESHOLD).collect();
                    format!("{truncated} …[truncated]")
                }
            } else {
                content
            };

            results.push((similarity, model::Message::assistant(text)));
        }

        Ok(results)
    }
}

/// Format a reply for context: use summary if content is long and summary exists.
fn format_reply(row: &ConversationRow) -> Option<String> {
    let content = row.reply_content.as_deref()?;
    if content.chars().count() > SUMMARY_THRESHOLD {
        if let Some(summary) = row.reply_summary.as_deref()
            && !summary.is_empty()
        {
            return Some(format!("[Summary] {summary}"));
        }
        let truncated: String = content.chars().take(SUMMARY_THRESHOLD).collect();
        return Some(format!("{truncated} …[truncated]"));
    }
    Some(content.to_string())
}
