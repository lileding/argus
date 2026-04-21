//! Context curation: build the model message list from conversation history.
//!
//! Sliding window (recent turns) + semantic recall (similar past messages).
//! Long replies use their summary. Recalled messages are deduped against window.

use std::collections::HashSet;

use tracing::debug;

use super::embedding::EmbeddingClient;
use crate::database::Database;
use crate::upstream::types as model;

/// Thresholds for semantic recall.
const RECALL_SIMILARITY_THRESHOLD: f64 = 0.30;
const RECALL_BYTES_BUDGET: usize = 6000;

/// Build orchestrator context: system prompt + recalled + recent + current message.
pub(super) async fn build_context(
    db: &Database,
    embedder: Option<&EmbeddingClient>,
    system_prompt: &str,
    channel: &str,
    user_text: &str,
    context_window: usize,
) -> Vec<model::Message> {
    let mut messages = vec![model::Message::system(system_prompt)];

    // Semantic recall (if embedder available).
    let mut recalled_ids = HashSet::new();
    if let Some(embedder) = embedder
        && let Ok(vec) = embedder.embed_one(user_text).await
        && let Ok(results) = db.conversation.search(&vec, channel, 10).await
    {
        let mut total_bytes = 0;
        for (similarity, user_msg, reply_msg) in &results {
            if *similarity < RECALL_SIMILARITY_THRESHOLD {
                continue;
            }
            let size = user_msg.content.len() + reply_msg.as_ref().map_or(0, |m| m.content.len());
            if total_bytes + size > RECALL_BYTES_BUDGET {
                continue;
            }
            total_bytes += size;
            messages.push(user_msg.clone());
            if let Some(reply) = reply_msg {
                messages.push(reply.clone());
            }
            recalled_ids.insert(user_msg.content.clone());
        }
        debug!(
            recalled = messages.len() - 1,
            total_bytes, "semantic recall done"
        );
    }

    // Sliding window: recent conversation turns.
    if let Ok(recent) = db.conversation.recent(channel, context_window as i64).await {
        for msg in &recent {
            // Dedup: skip if already in recalled context.
            if msg.role == model::Role::User && recalled_ids.contains(&msg.content) {
                continue;
            }
            messages.push(msg.clone());
        }
    }

    // Current user message (always last).
    messages.push(model::Message::user(user_text));

    messages
}
