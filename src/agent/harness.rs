//! Context curation: build the model message list from conversation history.
//!
//! Sliding window (recent turns) + semantic recall (similar past messages).
//! Long replies use their summary. Recalled messages are deduped against window.

use std::collections::HashSet;

use tracing::debug;

use crate::database::Database;
use crate::upstream::types as model;

/// Thresholds for semantic recall.
const RECALL_SIMILARITY_THRESHOLD: f64 = 0.50;
const RECALL_BYTES_BUDGET: usize = 6000;

/// Build orchestrator context: system prompt + recalled + recent + current message.
/// `exclude_msg_id` is the current message's DB ID (excluded from queries to avoid self-reference).
/// `image_parts` are multimodal content parts for the current message (base64-encoded images).
#[allow(clippy::too_many_arguments)]
pub(super) async fn build_context(
    db: &Database,
    embedder: Option<&dyn super::EmbedService>,
    system_prompt: &str,
    channel: &str,
    user_text: &str,
    exclude_msg_id: Option<i64>,
    context_window: usize,
    image_parts: Vec<model::ContentPart>,
) -> Vec<model::Message> {
    // Build system prompt with pinned memories.
    let system_content = if let Ok(memories) = db.memories.list_active().await {
        if memories.is_empty() {
            system_prompt.to_string()
        } else {
            let memory_lines: Vec<String> = memories
                .iter()
                .map(|m| format!("- [{}] {}", m.category, m.content))
                .collect();
            format!(
                "{}\n\n## User Memories\n\n{}",
                system_prompt,
                memory_lines.join("\n")
            )
        }
    } else {
        system_prompt.to_string()
    };
    let mut messages = vec![model::Message::system(system_content)];

    // Semantic recall (if embedder available).
    // Search both user messages AND agent replies — the answer to "what did I say
    // about X" is often in the agent's reply, not the user's original message.
    let mut recalled_ids = HashSet::new();
    let mut recalled_replies = HashSet::new();
    if let Some(embedder) = embedder
        && let Ok(vec) = embedder.embed_one(user_text).await
    {
        let mut total_bytes = 0;

        // 1. Search user messages (conversation view).
        if let Ok(results) = db
            .conversation
            .search(&vec, channel, exclude_msg_id, 10)
            .await
        {
            for (row_id, similarity, user_msg, reply_msg) in &results {
                if *similarity < RECALL_SIMILARITY_THRESHOLD {
                    continue;
                }
                let size =
                    user_msg.content.len() + reply_msg.as_ref().map_or(0, |m| m.content.len());
                if total_bytes + size > RECALL_BYTES_BUDGET {
                    break;
                }
                total_bytes += size;
                messages.push(user_msg.clone());
                if let Some(reply) = reply_msg {
                    recalled_replies.insert(reply.content.clone());
                    messages.push(reply.clone());
                }
                recalled_ids.insert(*row_id);
            }
        }

        // 2. Search agent replies (notifications table), dedup against conversation results.
        if let Ok(results) = db.conversation.search_replies(&vec, 10).await {
            for (similarity, reply_msg) in &results {
                if *similarity < RECALL_SIMILARITY_THRESHOLD {
                    continue;
                }
                if recalled_replies.contains(&reply_msg.content) {
                    continue;
                }
                let size = reply_msg.content.len();
                if total_bytes + size > RECALL_BYTES_BUDGET {
                    break;
                }
                total_bytes += size;
                messages.push(reply_msg.clone());
            }
        }

        debug!(
            recalled = recalled_ids.len(),
            total_bytes, "semantic recall done"
        );
    }

    // Sliding window: recent conversation turns (excluding current message).
    if let Ok(recent) = db
        .conversation
        .recent(channel, exclude_msg_id, context_window as i64)
        .await
    {
        for (row_id, user_msg, reply_msg) in &recent {
            // Dedup by row ID: skip if already in recalled context.
            if recalled_ids.contains(row_id) {
                continue;
            }
            messages.push(user_msg.clone());
            if let Some(reply) = reply_msg {
                messages.push(reply.clone());
            }
        }
    }

    // Current user message (always last). Include images if present.
    if image_parts.is_empty() {
        messages.push(model::Message::user(user_text));
    } else {
        messages.push(model::Message::user_with_images(user_text, image_parts));
    }

    messages
}
