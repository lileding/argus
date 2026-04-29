mod harness;
mod processor;
pub(crate) mod skill;
mod tool;

use std::collections::HashMap;
use std::path::Path;
use std::sync::Arc;
use std::sync::atomic::AtomicU32;

use tokio::sync::{Mutex, mpsc, oneshot};
use tokio_util::sync::CancellationToken;
use tracing::{debug, info, warn};

use crate::database::Database;
use crate::upstream;
use crate::upstream::types as model;

use processor::Processor;

// --- Type aliases ---

/// A channel_id (DB primary key on `channels.id`). The default channel is
/// represented as `Option::None` everywhere — never `0` or any sentinel.
pub(crate) type Channel = i64;

/// IM endpoint string (e.g. `feishu:p2p:ou_xxx`). The single source of truth
/// for routing inbound messages.
pub(crate) type Sink = String;

// --- Public types ---

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

/// Inbound user message (Gateway → Agent).
pub(crate) struct Message {
    pub(crate) msg_id: String,
    /// Originating sink (IM endpoint string).
    pub(crate) sink: Sink,
    /// Channel ID for tenant isolation. Stamped by the Agent dispatcher
    /// from the live routing table; whatever the producer sets here is
    /// overwritten before forwarding to a Processor.
    pub(crate) channel_id: Option<Channel>,
    /// Database row ID (None if DB save failed).
    pub(crate) db_msg_id: Option<i64>,
    pub(crate) ready: oneshot::Receiver<Payload>,
    pub(crate) port: mpsc::Sender<Notification>,
}

/// Outbound notification (Agent → Gateway). The Gateway dispatches to the
/// matching IM adapter by parsing `sink`'s prefix (e.g. `feishu:...`).
pub(crate) struct Notification {
    /// Destination sink (IM endpoint string). Gateway uses this for routing.
    pub(crate) sink: Sink,
    pub(crate) msg_id: String,
    /// Database message ID to link the notification to (None for async task results).
    pub(crate) db_msg_id: Option<i64>,
    /// Channel ID stamped by the Agent. Gateway forwards it to
    /// `save_notification`, which atomically backfills `messages.channel_id`
    /// in the same transaction as `reply_id`.
    pub(crate) channel_id: Option<Channel>,
    /// Agent emits events; frontend consumes them to drive UI.
    /// Dropping the sender signals "notification complete".
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

/// Specification for an async background task.
pub(crate) struct TaskSpec {
    pub(crate) id: u32,
    pub(crate) goal: String,
    /// Originating sink (used for the completion notification's return path).
    pub(crate) sink: Sink,
    /// Channel ID — drives Processor selection. For cron-fired tasks this
    /// reflects the channel the cron was created in (history identity);
    /// the sink's current routing is irrelevant for this lookup.
    pub(crate) channel_id: Option<Channel>,
    pub(crate) msg_id: String,
    pub(crate) port: mpsc::Sender<Notification>,
    pub(crate) source: TaskSource,
}

/// Where a TaskSpec originated. Affects the completion notification header.
pub(crate) enum TaskSource {
    /// Created by the orchestrator via the create_task tool.
    User,
    /// Triggered by a cron schedule.
    Cron { cron_id: i64 },
}

// --- Constants & prompts shared with processor ---

/// Async task budgets: 3× sync defaults.
pub(super) const TASK_TOOL_BUDGETS: &[(&str, usize)] = &[
    ("search", 9),
    ("fetch", 12),
    ("cli", 15),
    ("db", 18),
    ("write_file", 9),
    ("remember", 3),
    ("search_history", 6),
];

/// Thinking/reasoning token budget for async tasks.
pub(super) const TASK_THINKING_BUDGET: usize = 10000;

/// Max iterations for async tasks (3× sync default of 10).
pub(super) const TASK_MAX_ITERATIONS: usize = 30;

pub(super) const ORCHESTRATOR_PROMPT: &str = r#"You are the ORCHESTRATOR of an AI agent. Your job is to gather information using tools, then call finish_task with a summary.

RULES:
- You MUST call tools. Text output is ignored — only tool calls matter.
- The conversation history and user memories are already in your context. If the answer is there (e.g. user's preferences, facts, past discussions), call finish_task immediately with a summary. No need to search.
- If Available Skills are listed below and the user's request matches one, call activate_skill FIRST to load its instructions, then follow them (e.g. use the db tool as the skill directs).
- For questions requiring external information, call search, fetch, read_file, or other tools first.
- When you have enough material, call finish_task with a brief summary of what you found.
- For opinions or reviews, search from 2-3 different angles for comprehensive coverage.
- Do NOT answer from training knowledge alone — use tools to verify real-time facts.
- Do NOT call the same tool with the same arguments twice.
- For requests that need deep research, comprehensive reports, multi-step analysis, or code generation — call create_task instead of doing it yourself. create_task runs a background worker with 3× your tool budget. Use it when the user explicitly asks for a thorough/detailed report, or when the work clearly exceeds a quick answer.
- For recurring/periodic requests ("every day at...", "remind me when...", "每天下午...") — call create_cron with a 6-field cron expression and a self-contained execution prompt. Use list_crons to show existing schedules, cancel_cron to stop one, update_cron to modify. Cron firings reuse the original message thread for replies."#;

pub(super) const TASK_ORCHESTRATOR_PROMPT: &str = r#"You are a RESEARCH WORKER executing a background task. Your job is to thoroughly research the given goal using tools, then call finish_task with the FINAL REPORT.

CRITICAL: Your finish_task summary is delivered DIRECTLY to the user as the final output. It must be a complete, well-structured, publishable document — not a brief summary. Write it as if it's the finished deliverable.

RULES:
- You MUST call tools. Text output is ignored — only tool calls matter.
- Search from multiple angles (3-5 different queries) for comprehensive coverage.
- Fetch and read primary sources — don't rely on search snippets alone.
- Cross-reference facts across multiple sources.
- When you have gathered thorough materials, call finish_task with the COMPLETE REPORT:
  - Use markdown formatting: headings, tables, lists, code blocks
  - Include all key facts, data points, analysis, and conclusions
  - Cite sources with URLs
  - Match the user's language
- Do NOT answer from training knowledge alone — use tools to verify facts.
- Do NOT call the same tool with the same arguments twice.
- You have a large tool budget — use it. Be thorough, not quick."#;

pub(super) const SYNTHESIZER_PROMPT: &str = r#"You are the SYNTHESIZER. You receive the user's question and materials gathered by the orchestrator (tool results + summary). Compose a clear, helpful answer.

RULES:
- Base your answer ONLY on the provided materials. Do not add facts from training knowledge.
- Match the user's language and tone. If they asked in Chinese, answer in Chinese.
- Use markdown formatting: headings, lists, code blocks.
- Be concise, well-structured, and directly address the user's question.
- Cite sources when available (include URLs from search results)."#;

/// Tool call budgets: max calls per tool per orchestrator run.
pub(super) const TOOL_BUDGETS: &[(&str, usize)] = &[
    ("search", 3),
    ("fetch", 4),
    ("cli", 5),
    ("db", 6),
    ("write_file", 3),
    ("remember", 3),
    ("search_history", 2),
];

/// Max bytes per tool result before truncation.
pub(super) const MAX_TOOL_RESULT_BYTES: usize = 16 * 1024;

/// Max text tokens before early abort (orchestrator should only call tools).
pub(super) const EARLY_ABORT_TOKENS: usize = 80;

/// Max budget rejections before force-stopping.
pub(super) const MAX_BUDGET_REJECTIONS: usize = 5;

// --- Agent dispatcher ---

/// Agent is a thin dispatcher: it holds only the external entry mpscs and
/// references to its dependencies. The routing table and Processor instances
/// are built when `run()` starts (DB upsert happens there) and live on
/// `run()`'s stack for the duration of the process.
///
/// Keeping `new()` synchronous matches the rest of the peer services and
/// lets `main.rs` wire everything before any I/O happens.
pub(crate) struct Agent<'a, E: EmbedService> {
    msg_tx: mpsc::Sender<Message>,
    msg_rx: Mutex<mpsc::Receiver<Message>>,
    task_tx: mpsc::Sender<TaskSpec>,
    task_rx: Mutex<mpsc::Receiver<TaskSpec>>,
    config: &'a crate::config::AgentConfig,
    upstream_reg: &'a upstream::Upstream,
    db: &'a Database,
    embed_service: &'a E,
    workspace_dir: &'a Path,
    next_task_id: &'a AtomicU32,
}

impl<'a, E: EmbedService> Agent<'a, E> {
    pub(crate) fn new(
        config: &'a crate::config::AgentConfig,
        upstream_reg: &'a upstream::Upstream,
        db: &'a Database,
        embed_service: &'a E,
        workspace_dir: &'a Path,
        next_task_id: &'a AtomicU32,
    ) -> Self {
        let (msg_tx, msg_rx) = mpsc::channel(64);
        let (task_tx, task_rx) = mpsc::channel(64);
        Self {
            msg_tx,
            msg_rx: Mutex::new(msg_rx),
            task_tx,
            task_rx: Mutex::new(task_rx),
            config,
            upstream_reg,
            db,
            embed_service,
            workspace_dir,
            next_task_id,
        }
    }

    pub(crate) async fn run(&self, cancel: &CancellationToken) {
        info!(
            orchestrator = self.config.orchestrator.model_name,
            synthesizer = self.config.synthesizer.model_name,
            embedding = self.embed_service.model_name(),
            max_iterations = self.config.max_iterations,
            routes = self.config.routes.len(),
            "agent initializing"
        );

        // Build the default Processor first — it must always exist. A failure
        // here is fatal; cancel the whole process so peer services stop too.
        let default = match self.build_processor(None) {
            Ok(p) => p,
            Err(e) => {
                tracing::error!(error = %e, "default processor build failed, aborting");
                cancel.cancel();
                return;
            }
        };

        let (routes, processors) = self.build_routes().await;

        info!("agent dispatcher started");
        let processors_run = futures::future::join_all(
            processors
                .values()
                .map(|p| async move { p.run(cancel).await }),
        );
        tokio::join!(
            self.dispatch_messages(&routes, &processors, &default, cancel),
            self.dispatch_tasks(&processors, &default, cancel),
            default.run(cancel),
            processors_run,
        );
        info!("agent dispatcher stopped");
    }

    /// Construct one Processor with the shared config and references.
    fn build_processor(
        &self,
        channel_id: Option<Channel>,
    ) -> Result<Processor<'a, E>, upstream::types::ClientError> {
        Processor::new(
            channel_id,
            self.config,
            self.upstream_reg,
            self.db,
            self.embed_service,
            self.workspace_dir,
            self.next_task_id,
        )
    }

    /// Upsert each configured route into the channels table, build a Processor
    /// per channel, and return the live routing table. DB-only channels (not in
    /// the current TOML) are warned about but preserved.
    async fn build_routes(
        &self,
    ) -> (
        HashMap<Sink, Channel>,
        HashMap<Channel, Arc<Processor<'a, E>>>,
    ) {
        let mut processors: HashMap<Channel, Arc<Processor<'a, E>>> = HashMap::new();
        let mut routes: HashMap<Sink, Channel> = HashMap::new();
        let mut configured_names: std::collections::HashSet<String> =
            std::collections::HashSet::new();

        for (name, route_cfg) in &self.config.routes {
            let channel_id = match self.db.channels.upsert(name, &route_cfg.sinks).await {
                Ok(id) => id,
                Err(e) => {
                    warn!(channel = name, error = %e, "channel upsert failed, skipping");
                    continue;
                }
            };
            configured_names.insert(name.clone());

            let processor = match self.build_processor(Some(channel_id)) {
                Ok(p) => p,
                Err(e) => {
                    warn!(channel = name, error = %e, "processor build failed, skipping");
                    continue;
                }
            };
            processors.insert(channel_id, Arc::new(processor));

            for sink in &route_cfg.sinks {
                if let Some(prev) = routes.insert(sink.clone(), channel_id) {
                    warn!(
                        sink,
                        previous = prev,
                        new = channel_id,
                        "sink listed under multiple channels, last wins"
                    );
                }
            }

            info!(
                channel = name,
                channel_id,
                sinks = route_cfg.sinks.len(),
                "channel route registered"
            );
        }

        match self.db.channels.list_all().await {
            Ok(all) => {
                for (id, name) in all {
                    if !configured_names.contains(&name) {
                        warn!(
                            channel = name,
                            channel_id = id,
                            "channel exists in DB but not in current config; existing rows preserved, no runtime route active"
                        );
                    }
                }
            }
            Err(e) => warn!(error = %e, "channels list_all failed"),
        }

        (routes, processors)
    }

    /// Read inbound Messages, stamp channel_id from current routing, forward
    /// to the matching Processor.
    async fn dispatch_messages(
        &self,
        routes: &HashMap<Sink, Channel>,
        processors: &HashMap<Channel, Arc<Processor<'a, E>>>,
        default: &Processor<'a, E>,
        cancel: &CancellationToken,
    ) {
        let mut rx = self.msg_rx.lock().await;
        loop {
            tokio::select! {
                msg = rx.recv() => {
                    let Some(mut msg) = msg else {
                        debug!("agent message channel closed");
                        break;
                    };
                    let channel_id = routes.get(&msg.sink).copied();
                    msg.channel_id = channel_id;
                    let proc = processor_for(processors, default, channel_id);
                    if let Err(e) = proc.msg_tx().send(msg).await {
                        warn!(error = %e, "forward to processor message channel failed");
                    }
                }
                _ = cancel.cancelled() => {
                    info!("dispatch_messages received shutdown signal");
                    break;
                }
            }
        }
    }

    /// Read TaskSpecs and forward to Processor by channel_id (NOT by sink —
    /// cron-fired tasks must reach the channel that owns the cron's history,
    /// even if the sink has since migrated).
    async fn dispatch_tasks(
        &self,
        processors: &HashMap<Channel, Arc<Processor<'a, E>>>,
        default: &Processor<'a, E>,
        cancel: &CancellationToken,
    ) {
        let mut rx = self.task_rx.lock().await;
        loop {
            tokio::select! {
                spec = rx.recv() => {
                    let Some(spec) = spec else {
                        debug!("agent task channel closed");
                        break;
                    };
                    let proc = processor_for(processors, default, spec.channel_id);
                    if let Err(e) = proc.task_tx().send(spec).await {
                        warn!(error = %e, "forward to processor task channel failed");
                    }
                }
                _ = cancel.cancelled() => {
                    info!("dispatch_tasks received shutdown signal");
                    break;
                }
            }
        }
    }

    /// External entry: clone for IM adapters to inject Messages.
    pub(crate) fn port(&self) -> mpsc::Sender<Message> {
        self.msg_tx.clone()
    }

    /// External entry: clone for Scheduler to inject cron-fired TaskSpecs.
    pub(crate) fn task_port(&self) -> mpsc::Sender<TaskSpec> {
        self.task_tx.clone()
    }
}

/// Pick the Processor for a given channel_id. None or unknown id → default.
fn processor_for<'b, 'a, E: EmbedService>(
    processors: &'b HashMap<Channel, Arc<Processor<'a, E>>>,
    default: &'b Processor<'a, E>,
    ch: Option<Channel>,
) -> &'b Processor<'a, E> {
    match ch {
        Some(c) => processors.get(&c).map(|a| a.as_ref()).unwrap_or(default),
        None => default,
    }
}

// --- Shared helpers (used by processor) ---

/// Extract the summary from finish_task arguments JSON.
pub(super) fn extract_summary(args_json: &str) -> String {
    serde_json::from_str::<serde_json::Value>(args_json)
        .ok()
        .and_then(|v| v.get("summary").and_then(|s| s.as_str()).map(String::from))
        .unwrap_or_default()
}

/// Truncate a string to max_bytes, respecting UTF-8 boundaries.
pub(super) fn truncate(s: &str, max_bytes: usize) -> String {
    if s.len() <= max_bytes {
        return s.to_string();
    }
    let mut end = max_bytes;
    while end > 0 && !s.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}…[truncated, {} bytes total]", &s[..end], s.len())
}

const IMAGE_EXTENSIONS: &[&str] = &["png", "jpg", "jpeg", "gif", "webp"];

/// Read image files from disk, base64-encode them, return as ContentPart::Image.
/// Non-image files and read failures are silently skipped.
pub(super) fn load_image_parts(media_dir: &Path, file_paths: &[String]) -> Vec<model::ContentPart> {
    use base64::Engine;

    let mut parts = Vec::new();
    for filename in file_paths {
        let ext = Path::new(filename)
            .extension()
            .and_then(|e| e.to_str())
            .unwrap_or("");
        if !IMAGE_EXTENSIONS
            .iter()
            .any(|&img_ext| ext.eq_ignore_ascii_case(img_ext))
        {
            continue;
        }
        let abs_path = media_dir.join(filename);
        match std::fs::read(&abs_path) {
            Ok(bytes) => {
                let media_type = match ext.to_ascii_lowercase().as_str() {
                    "jpg" | "jpeg" => "image/jpeg",
                    "png" => "image/png",
                    "gif" => "image/gif",
                    "webp" => "image/webp",
                    _ => continue,
                };
                let data = base64::engine::general_purpose::STANDARD.encode(&bytes);
                debug!(filename, size = bytes.len(), "image loaded for multimodal");
                parts.push(model::ContentPart::Image {
                    media_type: media_type.to_string(),
                    data,
                });
            }
            Err(e) => {
                warn!(filename, error = %e, "failed to read image file, skipping");
            }
        }
    }
    parts
}

#[cfg(test)]
mod tests {
    use super::*;

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
        assert!(result.starts_with(&"a".repeat(100)));
    }

    #[test]
    fn truncate_utf8_boundary() {
        let s = "你好世界abc";
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
