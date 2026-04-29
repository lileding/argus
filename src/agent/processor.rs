//! Per-channel processor: owns the orchestrator/synthesizer pair, the tool
//! registry, and the two work loops (sync messages + async tasks). Created
//! by the Agent dispatcher; one instance per logical channel (plus one
//! default for unrouted sinks).
//!
//! All borrowed dependencies (db, upstream registry, embed service, etc.)
//! live elsewhere and are passed in by reference. The processor only owns
//! its own internal mpsc pairs and its model clients.

use std::collections::HashMap;
use std::path::Path;
use std::pin::Pin;
use std::sync::atomic::AtomicU32;
use std::time::Instant;

use futures::StreamExt;
use futures::stream::FuturesUnordered;
use tokio::sync::{Mutex, mpsc};
use tokio_util::sync::CancellationToken;
use tracing::{debug, info, warn};

use super::skill;
use super::tool;
use super::{
    EARLY_ABORT_TOKENS, Event, MAX_BUDGET_REJECTIONS, MAX_TOOL_RESULT_BYTES, Message, Notification,
    ORCHESTRATOR_PROMPT, SYNTHESIZER_PROMPT, TASK_MAX_ITERATIONS, TASK_ORCHESTRATOR_PROMPT,
    TASK_THINKING_BUDGET, TASK_TOOL_BUDGETS, TOOL_BUDGETS, TaskSource, TaskSpec, extract_summary,
    load_image_parts, truncate,
};
use crate::agent::EmbedService;
use crate::database::Database;
use crate::upstream;
use crate::upstream::types as model;

/// Per-channel work runner. Owns its internal queues; the Agent dispatcher
/// pushes routed Messages and TaskSpecs into them.
pub(super) struct Processor<'a, E: EmbedService> {
    /// Identity: None = default channel, Some(id) = configured channel.
    /// Used purely for logging / tagging; routing decisions live in Agent.
    channel_id: Option<i64>,
    msg_tx: mpsc::Sender<Message>,
    msg_rx: Mutex<mpsc::Receiver<Message>>,
    /// Internal task queue. Used by both the Agent dispatcher (for cron-fired
    /// TaskSpecs routed to this channel) and the create_task tool inside
    /// this processor's own orchestrator.
    task_tx: mpsc::Sender<TaskSpec>,
    task_rx: Mutex<mpsc::Receiver<TaskSpec>>,
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

impl<'a, E: EmbedService> Processor<'a, E> {
    #[allow(clippy::too_many_arguments)]
    pub(super) fn new(
        channel_id: Option<i64>,
        config: &crate::config::AgentConfig,
        upstream_reg: &upstream::Upstream,
        db: &'a Database,
        embed_service: &'a E,
        workspace_dir: &'a Path,
        next_task_id: &'a AtomicU32,
    ) -> Result<Self, upstream::types::ClientError> {
        let orchestrator = upstream_reg.client_for(&config.orchestrator)?;
        let synthesizer = upstream_reg.client_for(&config.synthesizer)?;

        let (msg_tx, msg_rx) = mpsc::channel(64);
        let (task_tx, task_rx) = mpsc::channel(64);

        Ok(Processor {
            channel_id,
            msg_tx,
            msg_rx: Mutex::new(msg_rx),
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

    /// Sender for routed Messages — used by Agent dispatcher.
    pub(super) fn msg_tx(&self) -> &mpsc::Sender<Message> {
        &self.msg_tx
    }

    /// Sender for routed TaskSpecs — used by Agent dispatcher.
    pub(super) fn task_tx(&self) -> &mpsc::Sender<TaskSpec> {
        &self.task_tx
    }

    pub(super) async fn run(&self, cancel: &CancellationToken) {
        info!(channel_id = ?self.channel_id, "processor started");
        tokio::join!(self.sync_message_loop(cancel), self.async_task_loop(cancel));
        info!(channel_id = ?self.channel_id, "processor stopped");
    }

    /// Sequential message processing. Completes current message before
    /// checking cancel — guarantees no mid-processing abort.
    async fn sync_message_loop(&self, cancel: &CancellationToken) {
        let mut rx = self.msg_rx.lock().await;
        loop {
            tokio::select! {
                msg = rx.recv() => {
                    let Some(msg) = msg else {
                        debug!(channel_id = ?self.channel_id, "processor message channel closed");
                        break;
                    };
                    self.process_message(msg).await;
                }
                _ = cancel.cancelled() => {
                    info!(channel_id = ?self.channel_id, "sync_message_loop received shutdown signal");
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
                    info!(channel_id = ?self.channel_id, task_id = spec.id, thinking_budget = TASK_THINKING_BUDGET, "async task started");
                    tasks.push(Box::pin(self.run_task(spec)));
                }
                _ = tasks.next(), if !tasks.is_empty() => {}
                _ = cancel.cancelled() => {
                    info!(channel_id = ?self.channel_id, in_flight = tasks.len(), "async_task_loop shutdown, dropping tasks");
                    break;
                }
            }
        }
    }

    async fn process_message(&self, msg: Message) {
        let Message {
            msg_id,
            sink,
            channel_id,
            db_msg_id,
            ready,
            port,
        } = msg;
        info!(sink, channel_id, msg_id, "processing message");

        // Open events channel → frontend shows thinking card.
        let (events_tx, events_rx) = mpsc::channel(16);
        let notif = Notification {
            sink: sink.clone(),
            msg_id: msg_id.clone(),
            db_msg_id,
            channel_id,
            events: events_rx,
        };
        if port.send(notif).await.is_err() {
            warn!("outbound channel closed, dropping message");
            return;
        }
        debug!(sink, msg_id, "notification posted to outbound port");

        // Wait for payload with timeout (media processing may hang).
        let payload = match tokio::time::timeout(std::time::Duration::from_secs(120), ready).await {
            Ok(Ok(p)) => p,
            Ok(Err(_)) => {
                warn!(sink, msg_id, "ready channel dropped");
                return;
            }
            Err(_) => {
                warn!(sink, msg_id, "media processing timed out (120s)");
                return;
            }
        };
        debug!(
            sink,
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
        let mut messages = super::harness::build_context(
            self.db,
            Some(self.embed_service),
            &orch_prompt,
            channel_id,
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
                &sink,
                channel_id,
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
            info!(sink, msg_id, "task complete (async task created)");
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
                debug!(sink, msg_id, "synthesizer done, {} chars", text.len());
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
                    warn!(sink, msg_id, error = %e, "trace finalize failed");
                }
            }
            Err(e) => {
                warn!(sink, msg_id, error = %e, "synthesizer failed");
                let _ = events_tx
                    .send(Event::Reply {
                        text: format!("Error: {e}"),
                    })
                    .await;
            }
        }

        info!(sink, msg_id, "task complete");
    }

    /// Phase 1: Orchestrator tool loop with default budgets.
    #[allow(clippy::too_many_arguments)]
    async fn run_orchestrator(
        &self,
        messages: &mut Vec<model::Message>,
        sink: &str,
        channel_id: Option<i64>,
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
            sink,
            channel_id,
            msg_id,
            port,
            db_msg_id,
            events_tx,
            TOOL_BUDGETS,
            self.max_iterations,
            true,
            &model::ChatOptions::default(),
        )
        .await
    }

    #[allow(clippy::too_many_arguments)]
    async fn run_orchestrator_with_budgets(
        &self,
        messages: &mut Vec<model::Message>,
        sink: &str,
        channel_id: Option<i64>,
        msg_id: &str,
        port: &mpsc::Sender<Notification>,
        db_msg_id: Option<i64>,
        events_tx: &mpsc::Sender<Event>,
        tool_budgets: &[(&str, usize)],
        max_iterations: usize,
        include_create_task: bool,
        chat_options: &model::ChatOptions,
    ) -> (
        String,
        Vec<String>,
        i32,
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

        let mut trace = match db_msg_id {
            Some(mid) => {
                match self
                    .db
                    .traces
                    .begin(
                        mid,
                        channel_id,
                        &self.orch_model_name,
                        &self.synth_model_name,
                    )
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
            sink,
            channel_id,
            msg_id,
            port,
        };
        let mut all_tool_results: Vec<String> = Vec::new();
        let mut summary = String::new();
        let mut iterations: i32 = 0;

        for iteration in 0..max_iterations {
            iterations = (iteration + 1) as i32;
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

            let mut asst = model::Message::assistant(&resp.content);
            asst.reasoning_content = resp.reasoning_content.clone();
            asst.tool_calls = resp.tool_calls.clone();
            messages.push(asst);

            if let Some(ft) = resp.tool_calls.iter().find(|tc| tc.name == "finish_task") {
                summary = extract_summary(&ft.arguments);
                info!(summary_len = summary.len(), "finish_task called");
                break;
            }

            let mut futures = Vec::new();
            let mut rejected: Vec<(String, String, String)> = Vec::new();

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

            let results = futures::future::join_all(futures).await;

            for (id, name, err) in &rejected {
                messages.push(model::Message::tool_result(id, name, err));
            }

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
            let instant = model::ChatOptions::default();
            if let Ok(resp) = self
                .orchestrator
                .chat(messages, &finish_only, &instant)
                .await
            {
                if let Some(ft) = resp.tool_calls.iter().find(|tc| tc.name == "finish_task") {
                    summary = extract_summary(&ft.arguments);
                } else {
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

        let chat_options = model::ChatOptions {
            thinking_budget: TASK_THINKING_BUDGET,
        };

        let dummy_events = mpsc::channel(1).0;
        let (summary, _tool_results, _iterations, _trace) = self
            .run_orchestrator_with_budgets(
                &mut messages,
                &spec.sink,
                spec.channel_id,
                &spec.msg_id,
                &spec.port,
                None,
                &dummy_events,
                TASK_TOOL_BUDGETS,
                TASK_MAX_ITERATIONS,
                false,
                &chat_options,
            )
            .await;

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

        let (events_tx, events_rx) = mpsc::channel(4);
        let notif = Notification {
            sink: spec.sink,
            msg_id: spec.msg_id,
            db_msg_id: None,
            channel_id: spec.channel_id,
            events: events_rx,
        };
        if spec.port.send(notif).await.is_ok() {
            let _ = events_tx.send(Event::Reply { text: full_reply }).await;
        }
    }
}
