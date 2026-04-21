use pgvector::Vector;
use sqlx::{PgPool, Row};

use crate::upstream::types as model;

/// Max reply chars before using summary instead.
const SUMMARY_THRESHOLD: usize = 800;

/// A row from the conversation view.
struct ConversationRow {
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
    /// Returns model messages in chronological order (oldest first).
    pub(crate) async fn recent(
        &self,
        channel: &str,
        limit: i64,
    ) -> anyhow::Result<Vec<model::Message>> {
        let rows = sqlx::query(
            "SELECT user_content, reply_content, reply_summary \
             FROM conversation \
             WHERE channel = $1 \
             ORDER BY user_ts DESC \
             LIMIT $2",
        )
        .bind(channel)
        .bind(limit)
        .fetch_all(&self.pool)
        .await?;

        // Rows come newest-first; reverse to chronological.
        let mut messages = Vec::new();
        for row in rows.iter().rev() {
            let r = ConversationRow {
                user_content: row.get("user_content"),
                reply_content: row.get("reply_content"),
                reply_summary: row.get("reply_summary"),
            };
            messages.push(model::Message::user(&r.user_content));
            if let Some(reply) = format_reply(&r) {
                messages.push(model::Message::assistant(reply));
            }
        }

        Ok(messages)
    }

    /// Semantic search: find messages similar to the given embedding vector.
    /// Returns model messages (user + assistant pairs) from matching rows.
    pub(crate) async fn search(
        &self,
        embedding: &[f32],
        channel: &str,
        limit: i64,
    ) -> anyhow::Result<Vec<(f64, model::Message, Option<model::Message>)>> {
        let vec = Vector::from(embedding.to_vec());
        let rows = sqlx::query(
            "SELECT user_content, reply_content, reply_summary, \
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

        let mut results = Vec::new();
        for row in &rows {
            let similarity: f64 = row.get("similarity");
            let r = ConversationRow {
                user_content: row.get("user_content"),
                reply_content: row.get("reply_content"),
                reply_summary: row.get("reply_summary"),
            };
            let user_msg = model::Message::user(&r.user_content);
            let reply_msg = format_reply(&r).map(model::Message::assistant);
            results.push((similarity, user_msg, reply_msg));
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
        // No summary available — truncate.
        let truncated: String = content.chars().take(SUMMARY_THRESHOLD).collect();
        return Some(format!("{truncated} …[truncated]"));
    }
    Some(content.to_string())
}
