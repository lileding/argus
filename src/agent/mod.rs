mod harness;
pub(crate) mod skill;
mod tool;

use std::collections::HashMap;
use std::path::Path;
use std::time::Instant;

use tokio::sync::{Mutex, mpsc, oneshot};
use tokio_util::sync::CancellationToken;
use tracing::{debug, info, warn};

use crate::database::Database;
use crate::upstream;
use crate::upstream::types as model;

// --- Types ---

pub(crate) struct Payload {
    pub(crate) content: String,
    pub(crate) file_paths: Vec<String>,
}

/// Embedding service for semantic recall. Defined here so Agent never
/// imports Embedder. Embedder implements this; main wires them together.
#[async_trait::async_trait]
pub(crate) trait EmbedService: Send + Sync {
    async fn embed_one(
        &self,
        text: &str,
    ) -> Result<Vec<f32>, Box<dyn std::error::Error + Send + Sync>>;

    fn model_name(&self) -> &str;
}

pub(crate) struct Task {
    pub(crate) msg_id: String,
    /// Database channel (e.g. "feishu:p2p:ou_xxx").
    pub(crate) channel: String,
    /// Database row ID (None if DB not available).
    pub(crate) db_msg_id: Option<i64>,
    pub(crate) ready: oneshot::Receiver<Payload>,
    pub(crate) port: mpsc::Sender<Message>,
}

pub(crate) struct Message {
    pub(crate) msg_id: String,
    /// Agent emits events; frontend consumes them to drive UI.
    /// Dropping the sender signals "message complete".
    pub(crate) events: mpsc::Receiver<Event>,
}

pub(crate) enum Event {
    /// Tool status update. `tool` is the tool name (used as key for dedup —
    /// same tool name replaces previous line). `text` is the display string.
    ToolStatus { tool: String, text: String },
    /// Phase 2 starting — synthesizer composing the answer.
    Composing,
    /// Final reply — replaces entire card.
    Reply { text: String },
}

// --- Prompts ---

const ORCHESTRATOR_PROMPT: &str = r#"You are the ORCHESTRATOR of an AI agent. Your job is to gather information using tools, then call finish_task with a summary.

RULES:
- You MUST call tools. Text output is ignored — only tool calls matter.
- The conversation history and user memories are already in your context. If the answer is there (e.g. user's preferences, facts, past discussions), call finish_task immediately with a summary. No need to search.
- If Available Skills are listed below and the user's request matches one, call activate_skill FIRST to load its instructions, then follow them (e.g. use the db tool as the skill directs).
- For questions requiring external information, call search, fetch, read_file, or other tools first.
- When you have enough material, call finish_task with a brief summary of what you found.
- For opinions or reviews, search from 2-3 different angles for comprehensive coverage.
- Do NOT answer from training knowledge alone — use tools to verify real-time facts.
- Do NOT call the same tool with the same arguments twice."#;

const SYNTHESIZER_PROMPT: &str = r#"You are the SYNTHESIZER. You receive the user's question and materials gathered by the orchestrator (tool results + summary). Compose a clear, helpful answer.

RULES:
- Base your answer ONLY on the provided materials. Do not add facts from training knowledge.
- Match the user's language and tone. If they asked in Chinese, answer in Chinese.
- Use markdown formatting: headings, lists, code blocks.
- Be concise, well-structured, and directly address the user's question.
- Cite sources when available (include URLs from search results)."#;

/// Tool call budgets: max calls per tool per orchestrator run.
const TOOL_BUDGETS: &[(&str, usize)] = &[
    ("search", 3),
    ("fetch", 4),
    ("cli", 5),
    ("db", 6),
    ("write_file", 3),
    ("remember", 3),
    ("search_history", 2),
];

/// Max bytes per tool result before truncation.
const MAX_TOOL_RESULT_BYTES: usize = 16 * 1024;

/// Max text tokens before early abort (orchestrator should only call tools).
const EARLY_ABORT_TOKENS: usize = 80;

/// Max budget rejections before force-stopping.
const MAX_BUDGET_REJECTIONS: usize = 5;

// --- Agent ---

pub(crate) struct Agent<'a, E: EmbedService> {
    tx: mpsc::Sender<Task>,
    rx: Mutex<mpsc::Receiver<Task>>,
    db: &'a Database,
    orchestrator: Box<dyn upstream::Client>,
    synthesizer: Box<dyn upstream::Client>,
    embed_service: &'a E,
    context_window: usize,
    max_iterations: usize,
    workspace_dir: &'a Path,
    http: reqwest::Client,
    tavily_api_key: String,
    orch_model_name: String,
    synth_model_name: String,
    skill_index: skill::SkillIndex,
}

impl<'a, E: EmbedService> Agent<'a, E> {
    pub(crate) fn new(
        config: &crate::config::AgentConfig,
        upstream_reg: &upstream::Upstream,
        db: &'a Database,
        embed_service: &'a E,
        workspace_dir: &'a Path,
    ) -> Result<Self, upstream::types::ClientError> {
        let orchestrator = upstream_reg.client_for(&config.orchestrator)?;
        let synthesizer = upstream_reg.client_for(&config.synthesizer)?;

        info!(
            orchestrator = config.orchestrator.model_name,
            synthesizer = config.synthesizer.model_name,
            embedding = embed_service.model_name(),
            max_iterations = config.max_iterations,
            "agent initialized"
        );

        let (tx, rx) = mpsc::channel(64);
        Ok(Agent {
            tx,
            rx: Mutex::new(rx),
            db,
            orchestrator,
            synthesizer,
            embed_service,
            context_window: config.orchestrator_context_window,
            max_iterations: config.max_iterations,
            workspace_dir,
            http: reqwest::Client::new(),
            tavily_api_key: config.tavily_api_key.clone(),
            orch_model_name: config.orchestrator.model_name.clone(),
            synth_model_name: config.synthesizer.model_name.clone(),
            skill_index: skill::SkillIndex::load(workspace_dir),
        })
    }

    pub(crate) async fn run(&self, cancel: &CancellationToken) {
        info!("agent scheduler started");
        let mut rx = self.rx.lock().await;
        loop {
            tokio::select! {
                task = rx.recv() => {
                    let Some(task) = task else {
                        debug!("agent task channel closed");
                        break;
                    };
                    self.process_task(task).await;
                }
                _ = cancel.cancelled() => {
                    info!("agent received shutdown signal");
                    break;
                }
            }
        }
        info!("agent scheduler stopped");
    }

    async fn process_task(&self, task: Task) {
        let Task {
            msg_id,
            channel,
            db_msg_id,
            ready,
            port,
        } = task;
        info!(channel, msg_id, "processing task");

        // Open events channel → frontend shows thinking card.
        let (events_tx, events_rx) = mpsc::channel(16);
        let msg = Message {
            msg_id: msg_id.clone(),
            events: events_rx,
        };
        if port.send(msg).await.is_err() {
            warn!("outbound channel closed, dropping message");
            return;
        }
        debug!(channel, msg_id, "message posted to outbound port");

        // Wait for payload with timeout (media processing may hang).
        let payload = match tokio::time::timeout(std::time::Duration::from_secs(120), ready).await {
            Ok(Ok(p)) => p,
            Ok(Err(_)) => {
                warn!(channel, msg_id, "ready channel dropped");
                return;
            }
            Err(_) => {
                warn!(channel, msg_id, "media processing timed out (120s)");
                return;
            }
        };
        debug!(
            channel,
            msg_id,
            content_len = payload.content.len(),
            "payload received"
        );

        // Build orchestrator prompt with skill catalog appended.
        let catalog = self.skill_index.catalog();
        let orch_prompt = if catalog.is_empty() {
            ORCHESTRATOR_PROMPT.to_string()
        } else {
            format!("{ORCHESTRATOR_PROMPT}\n\n{catalog}")
        };

        // Build context with conversation history (semantic recall + sliding window).
        let mut messages = harness::build_context(
            self.db,
            Some(self.embed_service),
            &orch_prompt,
            &channel,
            &payload.content,
            db_msg_id,
            self.context_window,
        )
        .await;

        // Phase 1: Orchestrator tool loop.
        let (summary, tool_results, iterations, trace) = self
            .run_orchestrator(&mut messages, &channel, db_msg_id, &events_tx)
            .await;

        // Transition to Phase 2.
        let _ = events_tx.send(Event::Composing).await;

        // Phase 2: Synthesizer — stream the final answer to frontend.
        let reply_text = match self
            .run_synthesizer(&payload.content, &summary, &tool_results, &events_tx)
            .await
        {
            Ok((text, usage)) => {
                debug!(channel, msg_id, "synthesizer done");
                // Finalize trace with synthesizer stats.
                if let Some(trace) = trace {
                    let reply_id = self
                        .db
                        .notifications
                        .save_notification(db_msg_id, &text)
                        .await
                        .ok();
                    if let Err(e) = trace
                        .finalize(
                            iterations,
                            &summary,
                            reply_id,
                            usage.prompt_tokens,
                            usage.completion_tokens,
                        )
                        .await
                    {
                        warn!(channel, msg_id, error = %e, "trace finalize failed");
                    }
                }
                text
            }
            Err(e) => {
                warn!(channel, msg_id, error = %e, "synthesizer failed");
                let error_text = format!("Error: {e}");
                let _ = events_tx
                    .send(Event::Reply {
                        text: error_text.clone(),
                    })
                    .await;
                error_text
            }
        };

        // Persist reply if trace didn't already (no trace = no DB msg id).
        if db_msg_id.is_none() {
            let _ = self
                .db
                .notifications
                .save_notification(None, &reply_text)
                .await;
        }

        info!(channel, msg_id, "task complete");
    }

    /// Phase 1: Orchestrator tool loop. Calls model with tools, executes tool
    /// calls, appends results, loops until finish_task or max iterations.
    async fn run_orchestrator(
        &self,
        messages: &mut Vec<model::Message>,
        channel: &str,
        db_msg_id: Option<i64>,
        events_tx: &mpsc::Sender<Event>,
    ) -> (
        String,      // summary
        Vec<String>, // tool_results
        i32,         // iterations
        Option<crate::database::traces::TraceBuilder>,
    ) {
        let registry = tool::build_registry(
            self.db,
            self.embed_service,
            self.workspace_dir,
            &self.http,
            &self.tavily_api_key,
            &self.skill_index,
        );

        let tool_defs: Vec<model::ToolDef> = registry
            .iter()
            .map(|t| model::ToolDef {
                name: t.name().to_string(),
                description: t.description().to_string(),
                parameters: t.parameters(),
            })
            .collect();

        debug!(
            tool_count = tool_defs.len(),
            tools = ?tool_defs.iter().map(|t| t.name.as_str()).collect::<Vec<_>>(),
            "orchestrator tools registered"
        );

        // Begin trace.
        let mut trace = match db_msg_id {
            Some(mid) => {
                match self
                    .db
                    .traces
                    .begin(mid, channel, &self.orch_model_name, &self.synth_model_name)
                    .await
                {
                    Ok(t) => Some(t),
                    Err(e) => {
                        warn!(error = %e, "trace begin failed");
                        None
                    }
                }
            }
            None => None,
        };

        let mut budgets: HashMap<&str, usize> = TOOL_BUDGETS.iter().copied().collect();
        let mut budget_rejections: usize = 0;
        let tool_ctx = tool::ToolContext { channel };
        let mut all_tool_results: Vec<String> = Vec::new();
        let mut summary = String::new();
        let mut iterations: i32 = 0;

        for iteration in 0..self.max_iterations {
            iterations = (iteration + 1) as i32;
            // Call orchestrator with early abort (text-only detection).
            let resp = match self
                .orchestrator
                .chat_with_early_abort(messages, &tool_defs, EARLY_ABORT_TOKENS)
                .await
            {
                Ok(r) => r,
                Err(e) => {
                    warn!(iteration, error = %e, "orchestrator call failed");
                    summary = format!("Orchestrator error: {e}");
                    break;
                }
            };

            if let Some(ref mut t) = trace {
                t.add_usage(&resp.usage);
            }

            debug!(
                iteration,
                tool_calls = resp.tool_calls.len(),
                finish_reason = ?resp.finish_reason,
                "orchestrator response"
            );

            // No tool calls — retry on first iteration, otherwise use text as summary.
            if resp.tool_calls.is_empty() {
                if iteration == 0 {
                    messages.push(model::Message::assistant(&resp.content));
                    messages.push(model::Message::user(
                        "You MUST call a tool. Text output is ignored. \
                         Call search, fetch, read_file, or finish_task now.",
                    ));
                    continue;
                }
                summary = resp.content;
                break;
            }

            // Append assistant message with tool calls.
            let mut asst = model::Message::assistant(&resp.content);
            asst.tool_calls = resp.tool_calls.clone();
            messages.push(asst);

            // Check for finish_task sentinel.
            if let Some(ft) = resp.tool_calls.iter().find(|tc| tc.name == "finish_task") {
                summary = extract_summary(&ft.arguments);
                info!(summary_len = summary.len(), "finish_task called");
                break;
            }

            // Execute tools with budget enforcement.
            let mut futures = Vec::new();
            let mut rejected: Vec<(String, String, String)> = Vec::new(); // (id, name, error)

            for (seq, tc) in resp.tool_calls.iter().enumerate() {
                if let Some(remaining) = budgets.get_mut(tc.name.as_str()) {
                    if *remaining == 0 {
                        budget_rejections += 1;
                        let err = format!(
                            "error: {} budget exhausted. Call finish_task NOW with a summary.",
                            tc.name
                        );
                        rejected.push((tc.id.clone(), tc.name.clone(), err));
                        continue;
                    }
                    *remaining -= 1;
                }

                let tool = match registry.get(&tc.name) {
                    Some(t) => t,
                    None => {
                        rejected.push((
                            tc.id.clone(),
                            tc.name.clone(),
                            format!("error: unknown tool {}", tc.name),
                        ));
                        continue;
                    }
                };

                // Emit tool status to frontend card.
                let _ = events_tx
                    .send(Event::ToolStatus {
                        tool: tc.name.clone(),
                        text: tool.status_label(&tc.arguments),
                    })
                    .await;

                let tc_id = tc.id.clone();
                let tc_name = tc.name.clone();
                let tc_args = tc.arguments.clone();
                let normalized = tool.normalize_args(&tc.arguments);
                let seq = seq as i32;
                let iter = iteration as i32;

                futures.push(async move {
                    let start = Instant::now();
                    let result = tool.execute(&tool_ctx, &tc_args).await;
                    let duration_ms = start.elapsed().as_millis() as i32;
                    let is_error = result.starts_with("error:");
                    (
                        tc_id,
                        tc_name,
                        tc_args,
                        normalized,
                        result,
                        is_error,
                        duration_ms,
                        iter,
                        seq,
                    )
                });
            }

            // Execute concurrently.
            let results = futures::future::join_all(futures).await;

            // Append rejected tool results.
            for (id, name, err) in &rejected {
                messages.push(model::Message::tool_result(id, name, err));
            }

            // Append executed tool results.
            for (id, name, args, normalized, result, is_error, duration_ms, iter, seq) in &results {
                let truncated = truncate(result, MAX_TOOL_RESULT_BYTES);
                messages.push(model::Message::tool_result(id, name, &truncated));
                all_tool_results.push(format!("[{name}] {truncated}"));

                if let Some(ref t) = trace
                    && let Err(e) = t
                        .record_tool_call(
                            *iter,
                            *seq,
                            name,
                            args,
                            normalized,
                            &truncated,
                            *is_error,
                            *duration_ms,
                        )
                        .await
                {
                    warn!(error = %e, "record_tool_call failed");
                }

                debug!(
                    tool = name,
                    duration_ms,
                    is_error,
                    result_len = truncated.len(),
                    "tool executed"
                );
            }

            if budget_rejections >= MAX_BUDGET_REJECTIONS {
                summary = format!(
                    "(Orchestrator force-stopped after {} budget rejections.)",
                    budget_rejections
                );
                warn!(budget_rejections, "force-stopping orchestrator");
                break;
            }
        }

        if summary.is_empty() {
            summary = format!(
                "(Orchestrator reached max {} iterations; synthesizing from {} materials.)",
                self.max_iterations,
                all_tool_results.len()
            );
        }

        (summary, all_tool_results, iterations, trace)
    }

    /// Phase 2: Call the synthesizer model with streaming.
    /// Returns (full_reply_text, usage).
    async fn run_synthesizer(
        &self,
        user_text: &str,
        summary: &str,
        tool_results: &[String],
        events_tx: &mpsc::Sender<Event>,
    ) -> Result<(String, model::Usage), upstream::types::ClientError> {
        use futures::StreamExt;

        let materials = if tool_results.is_empty() {
            summary.to_string()
        } else {
            format!(
                "## Orchestrator Summary\n\n{}\n\n## Materials\n\n{}",
                summary,
                tool_results.join("\n\n---\n\n")
            )
        };

        let user_content = format!("{user_text}\n\n---\n\n{materials}");
        let messages = vec![
            model::Message::system(SYNTHESIZER_PROMPT),
            model::Message::user(user_content),
        ];

        let mut stream = self.synthesizer.chat_stream(&messages, &[]).await?;
        let mut full_reply = String::new();
        let mut usage = model::Usage::default();

        while let Some(chunk) = stream.next().await {
            if let Some(err) = &chunk.error {
                return Err(upstream::types::ClientError::Sse(err.clone()));
            }
            if !chunk.delta.is_empty() {
                full_reply.push_str(&chunk.delta);
            }
            if let Some(u) = chunk.usage {
                usage = u;
            }
            if chunk.done {
                break;
            }
        }

        let _ = events_tx
            .send(Event::Reply {
                text: full_reply.clone(),
            })
            .await;

        Ok((full_reply, usage))
    }

    /// Clone the task submission channel for use by IM adapters.
    pub(crate) fn port(&self) -> mpsc::Sender<Task> {
        self.tx.clone()
    }
}

/// Extract the summary from finish_task arguments JSON.
fn extract_summary(args_json: &str) -> String {
    serde_json::from_str::<serde_json::Value>(args_json)
        .ok()
        .and_then(|v| v.get("summary").and_then(|s| s.as_str()).map(String::from))
        .unwrap_or_default()
}

/// Truncate a string to max_bytes, respecting UTF-8 boundaries.
fn truncate(s: &str, max_bytes: usize) -> String {
    if s.len() <= max_bytes {
        return s.to_string();
    }
    let mut end = max_bytes;
    while end > 0 && !s.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}…[truncated, {} bytes total]", &s[..end], s.len())
}

#[cfg(test)]
mod tests {
    use super::*;

    // --- extract_summary ---

    #[test]
    fn extract_summary_valid() {
        let result = extract_summary(r#"{"summary": "found info"}"#);
        assert_eq!(result, "found info");
    }

    #[test]
    fn extract_summary_missing_field() {
        let result = extract_summary(r#"{"other": "x"}"#);
        assert_eq!(result, "");
    }

    #[test]
    fn extract_summary_invalid_json() {
        let result = extract_summary("not json");
        assert_eq!(result, "");
    }

    #[test]
    fn extract_summary_empty_object() {
        let result = extract_summary("{}");
        assert_eq!(result, "");
    }

    #[test]
    fn extract_summary_null_value() {
        let result = extract_summary(r#"{"summary": null}"#);
        assert_eq!(result, "");
    }

    // --- truncate ---

    #[test]
    fn truncate_short_string() {
        let s = "hello";
        let result = truncate(s, 100);
        assert_eq!(result, "hello");
    }

    #[test]
    fn truncate_exact_boundary() {
        let s = "hello";
        let result = truncate(s, 5);
        assert_eq!(result, "hello");
    }

    #[test]
    fn truncate_long_ascii() {
        let s = "a".repeat(200);
        let result = truncate(&s, 100);
        assert!(result.len() < s.len());
        assert!(result.contains("[truncated, 200 bytes total]"));
        // First 100 bytes should be preserved.
        assert!(result.starts_with(&"a".repeat(100)));
    }

    #[test]
    fn truncate_utf8_boundary() {
        // "你好世界abc" — each CJK char is 3 bytes.
        // Total: 4*3 + 3 = 15 bytes.
        let s = "你好世界abc";
        // Truncate at 7 bytes: falls inside "世" (bytes 6..9).
        // Should back up to byte 6 (end of "好").
        let result = truncate(s, 7);
        assert!(result.starts_with("你好"));
        assert!(result.contains("[truncated,"));
    }

    #[test]
    fn truncate_empty() {
        let result = truncate("", 100);
        assert_eq!(result, "");
    }
}
