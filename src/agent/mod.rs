mod harness;
pub(crate) mod skill;
mod tool;

use std::collections::HashMap;
use std::path::Path;
use std::pin::Pin;
use std::sync::atomic::AtomicU32;
use std::time::Instant;

use futures::StreamExt;
use futures::stream::FuturesUnordered;
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

/// Inbound user message (Gateway → Agent).
pub(crate) struct Message {
    pub(crate) msg_id: String,
    /// Database channel (e.g. "feishu:p2p:ou_xxx").
    pub(crate) channel: String,
    /// Database row ID (None if DB not available).
    pub(crate) db_msg_id: Option<i64>,
    pub(crate) ready: oneshot::Receiver<Payload>,
    pub(crate) port: mpsc::Sender<Notification>,
}

/// Outbound notification (Agent → Gateway).
pub(crate) struct Notification {
    pub(crate) msg_id: String,
    /// Database message ID to link the notification to (None for async task results).
    pub(crate) db_msg_id: Option<i64>,
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
    pub(crate) channel: String,
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

/// Async task budgets: 3× sync defaults.
const TASK_TOOL_BUDGETS: &[(&str, usize)] = &[
    ("search", 9),
    ("fetch", 12),
    ("cli", 15),
    ("db", 18),
    ("write_file", 9),
    ("remember", 3),
    ("search_history", 6),
];

/// Thinking/reasoning token budget for async tasks.
const TASK_THINKING_BUDGET: usize = 10000;

/// Max iterations for async tasks (3× sync default of 10).
const TASK_MAX_ITERATIONS: usize = 30;

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
- Do NOT call the same tool with the same arguments twice.
- For requests that need deep research, comprehensive reports, multi-step analysis, or code generation — call create_task instead of doing it yourself. create_task runs a background worker with 3× your tool budget. Use it when the user explicitly asks for a thorough/detailed report, or when the work clearly exceeds a quick answer.
- For recurring/periodic requests ("every day at...", "remind me when...", "每天下午...") — call create_cron with a 6-field cron expression and a self-contained execution prompt. Use list_crons to show existing schedules, cancel_cron to stop one, update_cron to modify. Cron firings reuse the original message thread for replies."#;

/// Orchestrator prompt for async background tasks — no create_task, no delegation.
/// The finish_task summary is delivered directly to the user as the final output.
const TASK_ORCHESTRATOR_PROMPT: &str = r#"You are a RESEARCH WORKER executing a background task. Your job is to thoroughly research the given goal using tools, then call finish_task with the FINAL REPORT.

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
    tx: mpsc::Sender<Message>,
    rx: Mutex<mpsc::Receiver<Message>>,
    /// Async task submission channel (create_task tool → async_task_loop).
    task_tx: mpsc::Sender<TaskSpec>,
    task_rx: Mutex<mpsc::Receiver<TaskSpec>>,
    /// Shared task ID counter (also used by Scheduler).
    next_task_id: &'a AtomicU32,
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
        next_task_id: &'a AtomicU32,
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
        let (task_tx, task_rx) = mpsc::channel(64);
        Ok(Agent {
            tx,
            rx: Mutex::new(rx),
            task_tx,
            task_rx: Mutex::new(task_rx),
            next_task_id,
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
        tokio::join!(self.sync_message_loop(cancel), self.async_task_loop(cancel),);
        info!("agent scheduler stopped");
    }

    /// Sequential message processing. Completes current message before
    /// checking cancel — guarantees no mid-processing abort.
    async fn sync_message_loop(&self, cancel: &CancellationToken) {
        let mut rx = self.rx.lock().await;
        loop {
            tokio::select! {
                msg = rx.recv() => {
                    let Some(msg) = msg else {
                        debug!("agent message channel closed");
                        break;
                    };
                    self.process_message(msg).await;
                }
                _ = cancel.cancelled() => {
                    info!("sync_message_loop received shutdown signal");
                    break;
                }
            }
        }
    }

    /// Drives async tasks in parallel via FuturesUnordered.
    /// On cancel, drops all in-flight tasks immediately.
    async fn async_task_loop(&self, cancel: &CancellationToken) {
        let mut task_rx = self.task_rx.lock().await;
        let mut tasks: FuturesUnordered<
            Pin<Box<dyn std::future::Future<Output = ()> + Send + '_>>,
        > = FuturesUnordered::new();
        loop {
            tokio::select! {
                spec = task_rx.recv() => {
                    let Some(spec) = spec else { break; };
                    info!(task_id = spec.id, thinking_budget = TASK_THINKING_BUDGET, "async task started");
                    tasks.push(Box::pin(self.run_task(spec)));
                }
                _ = tasks.next(), if !tasks.is_empty() => {}
                _ = cancel.cancelled() => {
                    info!(in_flight = tasks.len(), "async_task_loop shutdown, dropping tasks");
                    break;
                }
            }
        }
    }

    async fn process_message(&self, msg: Message) {
        let Message {
            msg_id,
            channel,
            db_msg_id,
            ready,
            port,
        } = msg;
        info!(channel, msg_id, "processing message");

        // Open events channel → frontend shows thinking card.
        let (events_tx, events_rx) = mpsc::channel(16);
        let notif = Notification {
            msg_id: msg_id.clone(),
            db_msg_id,
            events: events_rx,
        };
        if port.send(notif).await.is_err() {
            warn!("outbound channel closed, dropping message");
            return;
        }
        debug!(channel, msg_id, "notification posted to outbound port");

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

        // Load images from payload file_paths for multimodal context.
        let image_parts = load_image_parts(
            &self.workspace_dir.join(crate::config::MEDIA_DIR),
            &payload.file_paths,
        );

        // Build context with conversation history (semantic recall + sliding window).
        let mut messages = harness::build_context(
            self.db,
            Some(self.embed_service),
            &orch_prompt,
            &channel,
            &payload.content,
            db_msg_id,
            self.context_window,
            image_parts,
        )
        .await;

        // Phase 1: Orchestrator tool loop.
        let (summary, tool_results, iterations, trace) = self
            .run_orchestrator(
                &mut messages,
                &channel,
                &msg_id,
                &port,
                db_msg_id,
                &events_tx,
            )
            .await;

        // If the orchestrator created an async task, skip the synthesizer
        // and reply with the summary directly. The synthesizer would otherwise
        // hallucinate an answer without materials.
        let created_task = tool_results.iter().any(|r| r.starts_with("[create_task]"));

        if created_task {
            let _ = events_tx
                .send(Event::Reply {
                    text: summary.clone(),
                })
                .await;
            if let Some(trace) = trace {
                let _ = trace.finalize(iterations, &summary, None, 0, 0).await;
            }
            info!(channel, msg_id, "task complete (async task created)");
            return;
        }

        // Transition to Phase 2.
        let _ = events_tx.send(Event::Composing).await;

        // Phase 2: Synthesizer — stream the final answer to frontend.
        match self
            .run_synthesizer(&payload.content, &summary, &tool_results, &events_tx)
            .await
        {
            Ok((text, usage)) => {
                debug!(channel, msg_id, "synthesizer done, {} chars", text.len());
                // Finalize trace (reply_id set later by Gateway after delivery).
                if let Some(trace) = trace
                    && let Err(e) = trace
                        .finalize(
                            iterations,
                            &summary,
                            None,
                            usage.prompt_tokens,
                            usage.completion_tokens,
                        )
                        .await
                {
                    warn!(channel, msg_id, error = %e, "trace finalize failed");
                }
            }
            Err(e) => {
                warn!(channel, msg_id, error = %e, "synthesizer failed");
                let _ = events_tx
                    .send(Event::Reply {
                        text: format!("Error: {e}"),
                    })
                    .await;
            }
        }

        // Notification persistence is handled by Gateway after card delivery.
        info!(channel, msg_id, "task complete");
    }

    /// Phase 1: Orchestrator tool loop with default budgets.
    async fn run_orchestrator(
        &self,
        messages: &mut Vec<model::Message>,
        channel: &str,
        msg_id: &str,
        port: &mpsc::Sender<Notification>,
        db_msg_id: Option<i64>,
        events_tx: &mpsc::Sender<Event>,
    ) -> (
        String,
        Vec<String>,
        i32,
        Option<crate::database::traces::TraceBuilder>,
    ) {
        self.run_orchestrator_with_budgets(
            messages,
            channel,
            msg_id,
            port,
            db_msg_id,
            events_tx,
            TOOL_BUDGETS,
            self.max_iterations,
            true,                           // include create_task for sync messages
            &model::ChatOptions::default(), // instant mode
        )
        .await
    }

    /// Phase 1: Orchestrator tool loop. Calls model with tools, executes tool
    /// calls, appends results, loops until finish_task or max iterations.
    #[allow(clippy::too_many_arguments)]
    async fn run_orchestrator_with_budgets(
        &self,
        messages: &mut Vec<model::Message>,
        channel: &str,
        msg_id: &str,
        port: &mpsc::Sender<Notification>,
        db_msg_id: Option<i64>,
        events_tx: &mpsc::Sender<Event>,
        tool_budgets: &[(&str, usize)],
        max_iterations: usize,
        include_create_task: bool,
        chat_options: &model::ChatOptions,
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
            &self.task_tx,
            self.next_task_id,
            include_create_task,
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
            thinking = chat_options.thinking_budget > 0,
            thinking_budget = chat_options.thinking_budget,
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

        let mut budgets: HashMap<&str, usize> = tool_budgets.iter().copied().collect();
        let mut budget_rejections: usize = 0;
        let tool_ctx = tool::ToolContext {
            channel,
            msg_id,
            port,
        };
        let mut all_tool_results: Vec<String> = Vec::new();
        let mut summary = String::new();
        let mut iterations: i32 = 0;

        for iteration in 0..max_iterations {
            iterations = (iteration + 1) as i32;
            // Call orchestrator with early abort (text-only detection).
            let resp = match self
                .orchestrator
                .chat_with_early_abort(messages, &tool_defs, EARLY_ABORT_TOKENS, chat_options)
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

            // Append assistant message with tool calls and reasoning.
            let mut asst = model::Message::assistant(&resp.content);
            asst.reasoning_content = resp.reasoning_content.clone();
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
                let ctx = tool_ctx.clone();

                futures.push(async move {
                    let start = Instant::now();
                    let result = tool.execute(&ctx, &tc_args).await;
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
                warn!(budget_rejections, "force-stopping orchestrator");
                break;
            }
        }

        // If orchestrator didn't call finish_task, force one last call with
        // only finish_task available (no other tools, no reasoning) so the
        // model writes a report from gathered materials.
        if summary.is_empty() {
            info!(
                materials = all_tool_results.len(),
                "orchestrator didn't finish, forcing final report"
            );
            messages.push(model::Message::user(
                "Budget exhausted. You MUST call finish_task NOW with a complete, \
                 well-structured report based on ALL materials gathered above. \
                 This is your final output to the user.",
            ));
            let finish_only: Vec<model::ToolDef> = registry
                .iter()
                .filter(|t| t.name() == "finish_task")
                .map(|t| model::ToolDef {
                    name: t.name().to_string(),
                    description: t.description().to_string(),
                    parameters: t.parameters(),
                })
                .collect();
            let instant = model::ChatOptions::default(); // no reasoning
            if let Ok(resp) = self
                .orchestrator
                .chat(messages, &finish_only, &instant)
                .await
            {
                if let Some(ft) = resp.tool_calls.iter().find(|tc| tc.name == "finish_task") {
                    summary = extract_summary(&ft.arguments);
                } else {
                    // Model produced text instead of calling finish_task — use it.
                    summary = resp.content;
                }
            }
            if summary.is_empty() {
                summary = format!(
                    "(Orchestrator exhausted budget with {} materials but failed to produce a report.)",
                    all_tool_results.len()
                );
            }
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

        let mut stream = self
            .synthesizer
            .chat_stream(&messages, &[], &model::ChatOptions::default())
            .await?;
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

    /// Execute an async background task: orchestrator with thinking mode,
    /// no synthesizer — finish_task summary is the final deliverable.
    async fn run_task(&self, spec: TaskSpec) {
        let task_id = spec.id;
        let start = Instant::now();

        // Async tasks use a dedicated prompt — no delegation, no create_task.
        // Append skill catalog so the worker can activate skills (e.g. food-tracker
        // for schema lookup) instead of guessing table/column names.
        let catalog = self.skill_index.catalog();
        let task_prompt = if catalog.is_empty() {
            TASK_ORCHESTRATOR_PROMPT.to_string()
        } else {
            format!("{TASK_ORCHESTRATOR_PROMPT}\n\n{catalog}")
        };
        let mut messages = vec![
            model::Message::system(&task_prompt),
            model::Message::user(&spec.goal),
        ];

        // Async tasks always use thinking/reasoning mode.
        let chat_options = model::ChatOptions {
            thinking_budget: TASK_THINKING_BUDGET,
        };

        // Orchestrator tool loop with higher budgets, no create_task, thinking enabled.
        let dummy_events = mpsc::channel(1).0;
        let (summary, _tool_results, _iterations, _trace) = self
            .run_orchestrator_with_budgets(
                &mut messages,
                &spec.channel,
                &spec.msg_id,
                &spec.port,
                None,
                &dummy_events,
                TASK_TOOL_BUDGETS,
                TASK_MAX_ITERATIONS,
                false, // no create_task in async tasks
                &chat_options,
            )
            .await;

        // No synthesizer — the orchestrator's finish_task summary IS the final output.
        let duration = start.elapsed();
        info!(
            task_id,
            duration_ms = duration.as_millis() as u64,
            "async task completed"
        );

        let header = match &spec.source {
            TaskSource::User => format!("**[Task #{task_id} completed]**\n\n"),
            TaskSource::Cron { cron_id } => {
                format!("**[Task #{task_id} · Cron #{cron_id}]**\n\n")
            }
        };
        let full_reply = format!("{header}{summary}");

        // Send completion notification to Gateway (persistence handled by Gateway).
        let (events_tx, events_rx) = mpsc::channel(4);
        let notif = Notification {
            msg_id: spec.msg_id,
            db_msg_id: None, // async task result is not linked to a specific message
            events: events_rx,
        };
        if spec.port.send(notif).await.is_ok() {
            let _ = events_tx.send(Event::Reply { text: full_reply }).await;
        }
    }

    /// Clone the message submission channel for use by IM adapters.
    pub(crate) fn port(&self) -> mpsc::Sender<Message> {
        self.tx.clone()
    }

    /// Clone the async task submission channel for use by Scheduler.
    pub(crate) fn task_port(&self) -> mpsc::Sender<TaskSpec> {
        self.task_tx.clone()
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

/// Image file extensions recognized for multimodal injection.
const IMAGE_EXTENSIONS: &[&str] = &["png", "jpg", "jpeg", "gif", "webp"];

/// Read image files from disk, base64-encode them, return as ContentPart::Image.
/// Non-image files and read failures are silently skipped.
fn load_image_parts(media_dir: &Path, file_paths: &[String]) -> Vec<model::ContentPart> {
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
