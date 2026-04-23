# Argus — Personal Assistant Agent

## Core Idea

This is not a chatbot. It is a **personal assistant**.

One assistant, one memory, one timeline. The user never needs to "start a new session" — all interactions share a single continuous timeline.

---

## Interaction Modes

### Private Chat
- Full assistant mode with complete history
- Free conversation, no commands needed

### Group @mention
- Task-focused mode, responds only to @mentions

### Group Silent Listening
- Passive observation mode
- Records mentioned items, proactively reminds when relevant
- Not yet implemented (future work)

---

## Architecture

### Three Decoupled Layers

Argus is structured as three layers with strict import boundaries:

```
┌────────────────────────────────┐
│  Gateway                       │  IM adapters, media processing,
│  (Feishu; future Slack)        │  card rendering
└────────────────────────────────┘
              │  Task → mpsc
              ▼
┌────────────────────────────────┐
│  Agent                         │  task scheduler, two-phase execution,
│  (orchestrator + synthesizer)  │  trace persistence
└────────────────────────────────┘
              │  uses
              ▼
┌────────────────────────────────┐
│  Upstream                      │  LLM clients (OpenAI, Anthropic,
│  (model / tool / embed /       │  Gemini-via-OpenAI), embedding,
│   render / skill)              │  document RAG, skills
└────────────────────────────────┘
```

Import direction is **Gateway → Agent → Upstream**, never reversed.
The Agent does not `use` any Gateway type; each `Task` carries its own
`mpsc::Sender<Message>` port, so reply events route themselves back
to the originating IM adapter.

### Four Peer Services

`main.rs` creates four top-level services and runs them concurrently
via `tokio::join!`. All services share references (`&self`) — no
`Arc`, no `spawn`:

```rust
let upstream = Upstream::new(&config.upstream);
let embedder = Embedder::new(&config.embedder, &upstream, &db)?;
let agent = Agent::new(&config.agent, &upstream, &db, &embedder, &config.workspace_dir)?;
let gateway = Gateway::new(&config.gateway, agent.port(), &upstream, &db, &config.workspace_dir);
let recovery = Recovery::new(&db, &gateway);

tokio::join!(
    gateway.run(&cancel),
    agent.run(&cancel),
    embedder.run(&cancel),
    recovery.run(&cancel),
    shutdown_signal(&cancel),
);
```

| Service | Responsibility |
|---------|---------------|
| **Gateway** | Manages all IM adapters. Each adapter runs its own `select!` loop: WS inbound, outbound rendering, media processing, recovery replay — all in a single `FuturesUnordered` pool |
| **Agent** | Two sub-loops via `join!`: `sync_message_loop` processes messages sequentially; `async_task_loop` drives background tasks in `FuturesUnordered` |
| **Embedder** | Background worker: embeds unembedded rows (messages, notifications, memories, chunks), summarizes long assistant replies, ingests queued documents |
| **Recovery** | Scans for unreplied messages at startup and every 5 minutes, replays them through the Gateway |

### Message, Notification, Event

```rust
pub(crate) struct Message {               // Gateway → Agent (inbound)
    pub(crate) msg_id: String,
    pub(crate) channel: String,           // e.g. "feishu:p2p:ou_xxx"
    pub(crate) db_msg_id: Option<i64>,
    pub(crate) ready: oneshot::Receiver<Payload>,
    pub(crate) port: mpsc::Sender<Notification>,
}

pub(crate) struct Payload {
    pub(crate) content: String,
    pub(crate) file_paths: Vec<String>,
}

pub(crate) struct Notification {          // Agent → Gateway (outbound)
    pub(crate) msg_id: String,
    pub(crate) events: mpsc::Receiver<Event>,
}

pub(crate) enum Event {
    ToolStatus { tool: String, text: String },
    Composing,
    Reply { text: String },
}
```

`ready` is a `oneshot` that carries the processed payload directly —
no DB round-trip between Gateway and Agent after media is ready.
Dropping the `events` sender is the sole "this notification is done"
signal; no explicit `close()` method.

### Per-Message Lifecycle

```
Gateway inbound (select! loop)        Agent sync_message_loop
──────────────────────────────        ──────────────────────────
WS event arrives                      msg ← mpsc::recv
  dedup by message_id                 notif := Notification { events }
  INSERT raw message (ready=false)    msg.port.send(notif)
  Message{ready_rx} constructed       payload := msg.ready.await
  msg_tx.send(msg)                    execute(payload, events_tx)
  → returns MediaWork                 drop(events_tx)

FuturesUnordered                      Gateway render (same select!)
────────────────                      ──────────────────────────────
download bytes → DB(ready=true)       notif ← outbound mpsc
process → ready_tx.send(payload)      reply_message → thinking card
                                      for event in notif.events:
                                          update_message(card)
                                      finalize card
```

Agent calls `port.send(notif)` **before** waiting on `ready` — this
preserves the "thinking card appears instantly, even while audio is
transcribing" UX.

### Content State Machine

Two state signals on `messages`:

```
ready:       false → true             (content processing complete)
reply_id:    NULL → Some(id)          (reply saved to notifications)
```

- **ready=false**: raw content stored; media download or transcription
  pending. Feishu file resources may expire, so download is
  non-interruptible during shutdown.
- **ready=true**: content fully processed. Reproducible from local
  bytes — safe to re-run after crash.

### Single Active Card Per Chat (Sync Messages)

The Agent processes sync messages sequentially (single loop). The
Feishu adapter renders each Notification in a concurrent future within
its `FuturesUnordered` pool. Since the Agent never issues concurrent
sync notifications, at most one card is "open" per chat at any instant
for sync messages. Async task completion notifications open separate
cards (see Async Tasks below).

### Recovery (Crash Replay)

Recovery is a standalone peer service, not a Gateway feature. On
startup and every 5 minutes, it scans `messages WHERE reply_id IS NULL`
from the last 24 hours.

```
On startup:
  1. Recovery.scan() queries messages with no reply
  2. For each row: route by channel prefix (e.g. "feishu:..." → feishu IM)
  3. Gateway.replay(msg) sends through the IM's recover channel
  4. IM adapter receives, reprocesses if needed, constructs Message → agent
```

Recovery handles the state combinations:
- `ready=false`: re-run media processing from raw content (may fail if
  Feishu resource expired — logged, skipped)
- `ready=true`: content already processed, submit task directly

**Cards are not re-attached.** Feishu rejects `update` calls on older
message IDs after a process restart. Replay opens a fresh card per
replayed task.

### Shutdown

One `CancellationToken` notifies all services. Each service decides
how to exit based on its own semantics:

```
cancel.cancel()
    │
    ├── Gateway (each IM adapter):
    │     drain outbound queue, drain in-flight FuturesUnordered, return
    ├── Agent (two sub-loops via join!):
    │     sync_message_loop: current message completes naturally, then return
    │     async_task_loop: break immediately, drop in-flight tasks, return
    ├── Embedder:
    │     break interval loop, return
    └── Recovery:
          break scan loop, return
```

Gateway `select!` on cancel: drains remaining outbound messages with a
500ms timeout, then drains all in-flight futures (media + render).
Agent `sync_message_loop` completes whatever message is currently
mid-execution — no new messages are pulled. `async_task_loop` exits
immediately; in-flight async tasks are dropped (no persistence, no
retry in v1).

### Two-Phase Agent (Orchestrator + Synthesizer)

A single-phase agent asks the model to do too much at once — understand the
request, choose tools, evaluate results, AND compose the answer. Weaker local
models skip tool calls entirely and answer from training memory. Splitting
into two narrow roles dramatically improves reliability:

- **Orchestrator** only calls tools. Its system prompt forbids text answers
  ("Text output is ignored — only tool calls matter"). It loops until it
  calls `finish_task` with a summary, gets force-stopped by the harness
  (budget exhaustion or max iterations), or produces text after retry.
- **Synthesizer** only writes the answer. It receives the user question
  and a `Materials` block containing every tool result and the orchestrator's
  summary. No conversation history, no tools — it composes purely from what
  Phase 1 gathered. This keeps the synthesizer cheap and immune to context
  pollution.

Both phases use different model clients (e.g. Claude for orchestration,
Gemini for synthesis). The `run_synthesizer` method streams its output via
SSE-style chunks to the frontend.

### Async Tasks

Some user requests require long-running work (deep research, code
generation, multi-step analysis). These are executed as **async tasks**
that run in the background while the Agent continues processing new
messages.

#### Concurrency Model

`Agent::run()` uses `tokio::join!` to run two sub-loops as peers:

```rust
async fn run(&self, cancel: &CancellationToken) {
    tokio::join!(
        self.sync_message_loop(cancel),
        self.async_task_loop(cancel),
    );
}
```

- **`sync_message_loop`**: sequential message processing. Pops from
  `msg_rx`, runs orchestrator → synthesizer, one at a time. On cancel,
  finishes the current message then returns.
- **`async_task_loop`**: drives a `FuturesUnordered` pool of background
  tasks. Pops `TaskSpec` from `task_rx`, pushes `run_task` futures into
  the pool. On cancel, returns immediately (drops in-flight tasks).

Both loops yield at `.await` points. `join!` polls them cooperatively
within a single tokio task — when `sync_message_loop` awaits an HTTP
response, `join!` polls `async_task_loop`, which drives background
tasks forward. No `Arc`, no `tokio::spawn`.

#### Trigger: `create_task` Tool

The orchestrator decides whether a request needs async execution. When
it does, it calls the `create_task` tool:

```
create_task(goal: "Research Google 8th gen TPU architecture and write a report")
→ "Created task #35"
```

The tool assigns a globally unique, human-readable ID (AtomicU32,
displayed as `#1`, `#2`, …), queues a `TaskSpec` via `task_tx`, and
returns the ID. The orchestrator then calls `finish_task` with
"Created async task #35: …". The synthesizer replies to the user
confirming the task was created.

Task IDs reset on restart (no persistence in v1).

#### Task Execution

Each async task runs its own independent orchestrator → synthesizer
cycle, reusing the same model clients and tool set:

- The `goal` from `create_task` becomes the user message
- Higher iteration limit and tool budgets (3× sync defaults)
- No conversation history context (the task is self-contained)
- On completion (success or failure), sends a `Notification` through
  the original message's `port`, using the trigger message's `msg_id`
  for reply threading

```
TaskSpec {
    id: u32,                          // #35
    goal: String,                     // "Research Google TPU..."
    channel: String,                  // "feishu:p2p:ou_xxx"
    msg_id: String,                   // original trigger message
    port: mpsc::Sender<Notification>, // for completion notification
}
```

#### Completion Notification

When a task finishes, a `Notification` is sent to the Gateway:

- **Success**: `[Task #35 completed]\n\n{synthesizer output}`
- **Failure**: `[Task #35 failed]\n\n{error}`

The notification replies to the original trigger message, creating a
reply thread in Feishu. The Gateway renders it as a normal card
(thinking → reply).

#### v1 Limitations

- No persistence: tasks are lost on restart
- No retry: failure is final
- No user interaction: no progress queries, no cancellation
- No dedicated model config: reuses sync orchestrator/synthesizer

---

## Multimodal Input

Argus handles all Feishu message types natively:

| Message Type | Processing |
|-------------|-----------|
| **text** | Fast path: extract text from JSON, instant |
| **image** | Download to `.files/` + save file_path |
| **post** (rich text) | Extract text + download embedded images |
| **audio** | Download `.opus` → Whisper transcription (OpenAI-compatible endpoint) |
| **file** (PDF, docx, etc.) | Download to `.files/` → register for document RAG ingestion |

Media processing runs as async futures inside the IM adapter's
`FuturesUnordered` pool. The inbound path is fast: parse → dedup →
INSERT → `task_tx.send()` → return `MediaWork`. The media future sends
the processed `Payload` through `task.ready_tx` when complete. The
Agent opens the thinking card (via `task.port.send(msg)`) immediately
on task pop, then awaits `ready` until content is ready.

### Audio Pipeline

```
Feishu audio → download → save .opus to .files/
    → Whisper v3 transcription (OpenAI-compatible /v1/audio/transcriptions)
    → verbose_json response with avg_logprob per segment
    → confidence computed as mean(avg_logprob)
    → text sent to orchestrator
```

The transcription prompt includes domain vocabulary hints for:
- Technology terms (API, Kubernetes, Docker, LLM, MLX, vLLM, omlx)
- Finance terms (ETF, hedge fund, quantitative)
- Classical composers in Chinese/Latin (Chopin 肖邦, Beethoven 贝多芬, ...)

### Document RAG (Personal Knowledge Base)

Non-image files go through the document ingester:
- Small files (≤ 1MB): processed inline during upload (extract → chunk → save)
- Large files: saved as "pending", processed by the Embedder background worker

Text extraction:
- PDF: `pdftotext` (host CLI)
- DOCX: `python3 -c` with `python-docx`
- txt/md/csv/json/yaml/xml: read directly

Chunks: 1500 chars with 300-char overlap. Stored in `chunks` table with
pgvector embeddings (768-dim, async-filled by the Embedder worker).

Retrieval happens through two paths:

1. **Active retrieval.** The Orchestrator has two tools:
   - `list_docs` — lists all documents with status
   - `search_docs` — semantic search over chunks (query embedded on demand,
     matched against chunk embeddings via pgvector cosine distance)

2. **Passive recall.** The harness's `build_context` does NOT currently
   inject document chunks (only conversation history). Active retrieval
   via tools is the primary path.

---

## Harness Engineering (Context Curation)

**Core principle: context is a scarce, finite resource. Only high-signal content goes in.**

The LLM never sees raw conversation history. The orchestrator goes through a
curation pipeline; the synthesizer receives no history at all (only user
question + materials from Phase 1).

```
Agent pops a task, opens card via port, waits on ready for Payload
        ↓
[1] History Curation (orchestrator only)
    - Semantic recall: embed current message, pgvector search both
      user messages and agent replies (notifications table)
      ▸ similarity threshold (0.50), byte budget (6 KB)
      ▸ dedup against sliding window by row ID
    - Sliding window: recent N conversation turns (channel-scoped)
      ▸ each turn = user message + optional notification reply
    - Pinned memories from `memories` table (active only)
        ↓
[2a] Orchestrator Prompt                [2b] Synthesizer Prompt
     - ORCHESTRATOR_PROMPT (tool-only        - SYNTHESIZER_PROMPT (answer-only
       rules, loop prevention)                 rules, language matching)
     - Pinned memories appended              (no history, no skills/tools)
     - Skill catalog (name + description)
        ↓                                        ↓
[3] Assemble                            [3] Assemble
    [sys] + [recalled] + [recent] +         [sys] + [user + materials]
    [user]
        ↓                                        ↓
   chat_with_early_abort (with tools)       chat_stream (no tools)
```

### Conversation View

The `conversation` SQL view joins messages with their notification
replies, filtering to `ready=TRUE` only. This ensures the harness
never sees raw JSON content from unprocessed messages. Recall searches
both user messages (by embedding) and agent replies (notifications
table), deduped by content to avoid double-injection.

### Safety

- Tool results truncated to 16 KB max before entering context
- Messages saved BEFORE context assembly (prevents duplicates on retry/crash)
- Async embedding via the Embedder worker — saving is never blocked on embed
- Semantic recall results sorted by time, not similarity

---

## Tool Budgets & Loop Prevention

Prompt-layer rules are insufficient against weak-instruction-following
models. Argus enforces hard limits at the harness layer:

| Safeguard | Trigger | Action |
|-----------|---------|--------|
| **Per-tool budget** | `search`≤3, `fetch`≤4, `db`≤6, `cli`≤5, `write_file`≤3, `remember`≤3, `search_history`≤2 per turn | Harness rejects further calls without dispatching; returns error "budget exhausted, call finish_task NOW" |
| **Cumulative rejection strike-out** | 5 budget-exhausted rejections in a turn | Force-exit orchestrator, pass gathered materials to synthesizer |
| **Text-only early abort** | Streaming detects >80 tokens of text without tool calls | Cancel stream immediately (~1s), retry with enforcement prompt instead of waiting 10-30s for full generation |
| **Tool-only retry** | First iteration produces no tool calls after early abort | Inject "You MUST call a tool" user message, retry once |
| **Text fallback** | Still no tool calls after retry | Use model text as synthesis summary |
| **Max iterations** | Configurable ceiling (default 10) | Force-exit with gathered materials |

Worst-case wall-clock is bounded by budgets + early abort. A model that
ignores the tool-only rule is detected in ~1s (not 30s), retried once,
then force-synthesized with whatever materials exist.

---

## Performance Optimizations

### Parallel Tool Execution

When the orchestrator receives multiple tool calls in a single model
response (e.g. 2 searches + 1 fetch), they execute concurrently via
`futures::future::join_all`. Results are appended to the context in
the original order for deterministic history. `finish_task` is
pre-scanned before dispatch; budget checks run serially (shared state).

Latency: `max(tool_time)` instead of `sum(tool_time)`.

### chat_with_early_abort (Streaming Phase 1)

The orchestrator uses `chat_with_early_abort` instead of blocking `chat`.
This method opens an SSE stream and accumulates both content deltas
AND tool call deltas. Two exit paths:

- **Model calls tools**: tool_call deltas appear in the stream →
  accumulate until stream completes → return full Response with ToolCalls.
  Behaves identically to blocking chat.
- **Model outputs text only** (ignored tool-only rule): content text
  exceeds 80 estimated tokens with no tool_call delta seen → cancel
  stream immediately → return partial text → orchestrator retries.
  Saves 10-30s of wasted generation.

---

## Streaming

Phase 2 (synthesizer) streams its output:

- `client.chat_stream` returns a `Pin<Box<dyn Stream<Item = StreamChunk>>>` 
  of incremental deltas from an OpenAI-compatible streaming endpoint
- Agent emits `Event::Reply` with the full accumulated text
- Feishu adapter renders as interactive card updates
- During streaming, LaTeX rendering is not applied — the final
  `Event::Reply` triggers full processing (LaTeX → PNG upload →
  `![](image_key)` embedding)

---

## Request Tracing

Every message processing is recorded in `traces` + `tool_calls` tables.

`traces`: one row per user-message → reply cycle. Fields: `message_id`,
`reply_id`, `chat_id`, `orchestrator_model`, `synthesizer_model`,
`iterations`, `summary`, prompt/completion tokens for both phases,
`duration_ms`.

`tool_calls`: one row per tool invocation. Fields: `trace_id`, `iteration`,
`seq`, `tool_name`, `arguments` (raw), `normalized_args` (parsed),
`result`, `is_error`, `duration_ms`.

The `TraceBuilder` is created at the start of orchestration, accumulates
usage from each orchestrator call, and is finalized with synthesizer
stats at the end.

---

## Model Strategy

### Why Commercial APIs

The original design used local models exclusively (Qwen 3.5 MoE on Mac
Studio). This worked for basic queries but had fundamental limitations:

- **Orchestrator quality**: local models frequently misused tools —
  calling skill names as tool names, using wrong column names in SQL,
  looping the same search 20+ times. The harness budgets, early-abort,
  and retry mechanisms were all built to survive these failures. With
  commercial models (even Haiku-class), these mechanisms rarely fire.
- **Synthesizer quality**: local models produced shallow, repetitive
  answers. Commercial models produce structured, insightful responses.
- **Cost reality**: at ~50-200 queries/day for a personal assistant,
  commercial API costs ($5-20/month) are negligible.

### Multi-Backend Architecture

Argus supports multiple model providers via named **upstreams**. Each
agent role (orchestrator, synthesizer, transcription, embedding,
summarization) selects its own upstream and model independently.

```toml
[upstream.local]
type = "openai"
base_url = "http://localhost:8000/v1"
api_key = "omlx"
timeout_secs = 240

[upstream.anthropic]
type = "anthropic"
api_key = "sk-ant-xxx"

[upstream.gemini]
type = "gemini"
api_key = "..."

[agent.orchestrator]
upstream = "anthropic"
model_name = "claude-haiku-4-5"

[agent.synthesizer]
upstream = "gemini"
model_name = "gemini-2.5-flash-lite"
max_tokens = 32768
```

Three upstream types, all implemented with `reqwest` (no SDKs):

| Type | Endpoint | Auth |
|------|----------|------|
| `openai` | Any OpenAI-compatible API | Bearer token |
| `anthropic` | Anthropic Messages API | `x-api-key` header |
| `gemini` | Google Gemini via OpenAI-compatible endpoint | API key (in URL or bearer) |

Gemini is accessed through Google's OpenAI-compatible endpoint
(`generativelanguage.googleapis.com/v1beta/openai`), reusing the
OpenAI client implementation.

### Recommended Configurations

| Orchestrator | Synthesizer | Cost | Quality |
|---|---|---|---|
| Claude Haiku 4.5 | Gemini 2.5 Flash Lite | ~$5/mo | Good quality, best value |
| GPT-5.4 | Gemini 2.5 Flash Lite | ~$15/mo | Excellent tool calling + fast synthesis |
| Qwen 3.5 (local) | Qwen 3.5 (local) | $0 | Functional but needs all harness guardrails |

### Local Models (Transcription + Embedding)

Local models serve two specialized roles:
- **Transcription**: Whisper Large v3 via OpenAI-compatible endpoint
- **Embedding**: modernbert-embed-base (768 dim) via OpenAI-compatible endpoint

---

## Output Rendering

All agent output is delivered as **Feishu interactive cards** (schema 2.0
with `update_multi: true`) so the same card can be PATCHed as state evolves:

1. **Thinking card** — opened on Message receive ("💭 正在思考...")
2. **Tool status card** — stacked lines per tool, deduped by tool name
   (e.g. "🔍 Searching: X"). New tools append; same tool updates its line.
3. **Composing card** — Phase 1→2 transition ("✏️ Composing answer...")
4. **Final reply card** — full markdown with code blocks as collapsible
   panels + LaTeX images

All cards are PATCHed onto the same card_id created when the adapter
opens the thinking card. Dropping the `events` channel signals the
render future to finalize.

### Card Building

- Code blocks (` ```lang...``` `) are extracted and rendered as Feishu
  `collapsible_panel` elements with the language name as header
- Non-code markdown is rendered as `markdown` elements
- Unclosed code blocks are treated as text

### LaTeX Rendering

Display LaTeX blocks (`$$...$$`) and inline (`$...$`) are detected via
regex. Display-mode blocks are rendered to PNG via **RaTeX** (pure Rust
crate — `ratex-parser`, `ratex-layout`, `ratex-render` with embedded
fonts), uploaded to Feishu as images, and inline-replaced with
`![](image_key)` markers. Inline LaTeX is detected but not rendered
to images (too small for useful image output).

---

## Skills

Skills follow the SKILL.md format (same as Claude Code agent skills).

### Loading

Skills are loaded **once at startup** from `{workspace_dir}/skills/*/SKILL.md`.
No hot-reload, no background rescan. The `SkillIndex` is an in-memory
`HashMap<String, SkillEntry>` built synchronously during `Agent::new`.

### SKILL.md Format

```
workspace/skills/
  calorie/
    SKILL.md
    setup.sql
  stock-analysis/
    SKILL.md
    scripts/
      fetch.py
```

```yaml
---
name: food-tracker
description: "记录、查询和分析每日饮食的热量与营养。"
tools:
  - db
---

## 饮食记录与营养分析

(table schema, command examples, nutrition estimation rules...)
```

If no YAML frontmatter is present, the directory name is used as the
skill name, and the first non-header paragraph as the description.

### Catalog Injection

Only skill name + description appear in the orchestrator's system
prompt (under "## Available Skills"). The full prompt is loaded on
demand when the orchestrator calls the `activate_skill` tool.

### Skill Lifecycle

Skills are authored and maintained by humans. The model can only read
them (via `activate_skill`), not create or modify them.

---

## Tools

| Tool | Purpose | Budget |
|------|---------|--------|
| `finish_task` | Sentinel — signals orchestrator → synthesizer transition | — |
| `current_time` | Date/time with timezone support | — |
| `search` | Web search (Tavily API, DuckDuckGo fallback) | 3/turn |
| `fetch` | URL → readable text (HTML stripped via `scraper` crate) | 4/turn |
| `read_file` | Read file contents (anywhere in workspace) | — |
| `write_file` | Write file (restricted to `.users/` directory) | 3/turn |
| `cli` | Execute shell commands on host (`bash -c` or `sh -c`) | 5/turn |
| `remember` | Pin a persistent memory (pgvector indexed) | 3/turn |
| `forget` | Deactivate a pinned memory by ID | — |
| `search_docs` | Semantic search over indexed document chunks | — |
| `list_docs` | List all indexed documents in knowledge base | — |
| `search_history` | Semantic search over conversation history | 2/turn |
| `db` | Structured data access (see below) | 6/turn |
| `activate_skill` | Load a skill's full instructions on demand | — |
| `create_task` | Create an async background task (returns task ID) | — |

### Tool Trait

Every tool implements a common trait:

```rust
#[async_trait]
trait Tool: Send + Sync {
    fn name(&self) -> &str;
    fn description(&self) -> &str;
    fn parameters(&self) -> serde_json::Value;  // JSON Schema
    async fn execute(&self, ctx: &ToolContext<'_>, args: &str) -> String;
    fn status_label(&self, args: &str) -> String;
    fn normalize_args(&self, args: &str) -> String;
}
```

The `ToolRegistry` holds `Vec<Box<dyn Tool + 'a>>` and is rebuilt per
task (tools borrow `&Database`, `&EmbedService`, etc. via lifetimes,
not `Arc`).

### Safety

- `read_file`: can read anywhere in workspace. Binary files return a
  description instead of raw bytes.
- `write_file`: **restricted to `.users/` directory**. The tool silently
  prepends `.users/` to any path. The model cannot overwrite skills
  (`.skills/`), media (`.files/`), or config files.
- `cli`: direct host execution (`bash -c`). No sandbox abstraction —
  runs on the host machine with no resource limits. Suitable for
  development on a personal Mac.
- `db`: structured CLI+JSON interface — the model never writes SQL.
- Tool output truncated to 16 KB to prevent context overflow.
- All tool calls logged with full arguments + normalized form for traces.

### Structured Data Access (db tool)

The model does not write SQL. Instead it sends CLI-style commands with
JSON values through a single `db` tool:

```
list                              — show all tables
describe food_log                 — show columns and types
create food_log {"meal_date": "date!", "calories": "number"}
query food_log where {"meal_date": "2026-04-17"} sort created_at desc limit 10
count food_log group_by ["meal_date"] sum ["calories"]
insert food_log {"meal_date": "2026-04-17", "food_name": "牛排", "calories": 450}
update food_log 42 {"calories": 550}
```

7 verbs: `list`, `describe`, `create`, `query`, `count`, `insert`, `update`.
No `delete` / `drop` / `truncate`. Data is append-only with in-place
correction.

**Namespace isolation**: the tool prepends `argus_` to all table names
internally. The model writes `food_log`; PG sees `argus_food_log`. System
tables (`messages`, `memories`, etc.) are unreachable because they don't
carry the prefix.

**Column validation**: all identifiers validated against regex
`^[a-z][a-z0-9_]{0,62}$`. Auto-managed columns (`id`, `created_at`,
`updated_at`) are rejected if specified by the model.

**Typed parameter casting**: before binding query parameters, the tool
looks up each column's PostgreSQL data type via `information_schema.columns`
and applies the correct SQL cast (`::date`, `::double precision`,
`::boolean`, etc.). This prevents type mismatch errors that cause query
failures with untyped string parameters.

**Query results**: returned as JSON arrays with proper type preservation
(integers, floats, booleans, dates, timestamps, JSONB all round-trip
correctly). `create` uses `CREATE TABLE` (not IF NOT EXISTS). `update`
only works by ID (no WHERE conditions).

**Types**: `text`, `number` (DOUBLE PRECISION), `date`, `boolean`,
`timestamp` (TIMESTAMPTZ), `json` (JSONB). Every table auto-gets
`id` + `created_at` + `updated_at`. Append `!` to a type for NOT NULL.

**Where operators**: exact match (default), `__gt`, `__gte`, `__lt`,
`__lte`, `__contains` (ILIKE), `__neq`. NULL-aware: `{"field": null}`
generates `IS NULL`, `{"field__neq": null}` generates `IS NOT NULL`.

**Batch insert**: accepts a JSON array of objects, wraps in a transaction.

**Query defaults**: `query` without `limit` defaults to 50 rows. Hard
cap is 200. `query` without `sort` defaults to `created_at DESC`.

---

## Media Storage

All downloaded media is saved to `{workspace_dir}/.files/`:

```
workspace/.files/
  img_v3_xxx.png          # Feishu images
  file_v3_xxx.opus        # Voice messages
  file_v3_xxx.pdf         # Uploaded files → ingested into pgvector
```

Files are named by their Feishu file key + original extension. Path
traversal is blocked: file keys containing `/`, `\`, `..`, or empty
strings are rejected.

---

## Memory

Argus has three memory layers, all active:

| Layer | Scope | Mechanism |
|-------|-------|-----------|
| **Sliding window** | Last `orchestrator_context_window` turns | Direct load from `conversation` view; user messages paired with notification replies |
| **Semantic recall** | All historical messages + replies | pgvector cosine search on the current message's embedding; searches both user messages and agent replies (notifications), deduped by content |
| **Pinned memories** | User-curated persistent notes | `memories` table; agent uses `remember`/`forget` tools; pgvector-indexed for recall; injected into system prompt |

### Embedder Worker

The `Embedder` runs two interval loops:
- **Embed interval** (default 30s): batch-embeds unembedded rows from
  `messages`, `notifications`, `chunks`, and `memories` tables. Also
  runs the document ingestion cycle (picks up "pending" documents).
- **Summary interval** (default 5 min): generates 2-3 sentence summaries
  for long, unsummarized assistant replies in `notifications`. Uses the
  synthesizer model.

### Notification Summaries

Long agent replies stored in `notifications` are summarized by the
Embedder worker. The `conversation` view exposes both `reply_content`
and `reply_summary`; the harness can use summaries to reduce context
pollution from verbose past replies. Summary failures leave
`summary IS NULL` — the notification stays in the unsummarized pool
and is retried next cycle.

---

## Data Model (PostgreSQL + pgvector)

After all migrations (001–007), the schema has evolved to separate user
messages from agent replies:

```sql
-- User messages (inbound only; assistant/tool rows deleted in migration 006)
CREATE TABLE messages (
    id              BIGSERIAL PRIMARY KEY,
    channel         TEXT NOT NULL DEFAULT '',   -- "feishu:p2p:ou_xxx" or "feishu:group:oc_xxx"
    content         TEXT NOT NULL,              -- raw content (JSON for non-text types)
    msg_type        TEXT NOT NULL DEFAULT 'text',
    file_paths      TEXT[],
    sender_id       TEXT,
    source_ts       TIMESTAMPTZ,
    trigger_msg_id  TEXT,                       -- IM message ID (for reply threading)
    embedding       vector(768),                -- async-filled by Embedder
    ready           BOOLEAN NOT NULL DEFAULT FALSE,  -- media processing complete
    reply_id        BIGINT REFERENCES notifications(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Agent replies (split from messages in migration 006)
CREATE TABLE notifications (
    id          BIGSERIAL PRIMARY KEY,
    message_id  BIGINT REFERENCES messages(id),
    content     TEXT NOT NULL,
    embedding   vector(768),
    summary     TEXT,                           -- async-generated by Embedder
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Conversation view: joins ready messages with their replies
CREATE VIEW conversation AS
SELECT m.id, m.channel, m.content AS user_content,
       m.embedding AS user_embedding, m.created_at AS user_ts,
       n.content AS reply_content, n.summary AS reply_summary,
       n.created_at AS reply_ts
FROM messages m
LEFT JOIN notifications n ON n.id = m.reply_id
WHERE m.ready = TRUE;

-- Agent-curated persistent memories
CREATE TABLE memories (
    id         BIGSERIAL PRIMARY KEY,
    content    TEXT NOT NULL,
    category   TEXT NOT NULL DEFAULT 'general',
    embedding  vector(768),
    active     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Document RAG
CREATE TABLE documents (
    id         BIGSERIAL PRIMARY KEY,
    filename   TEXT NOT NULL,
    file_path  TEXT NOT NULL,
    channel    TEXT,
    status     TEXT NOT NULL DEFAULT 'pending',  -- pending / processing / ready / error
    error_msg  TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE chunks (
    id          BIGSERIAL PRIMARY KEY,
    document_id BIGINT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    chunk_index INT NOT NULL,
    content     TEXT NOT NULL,
    embedding   vector(768),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Request tracing
CREATE TABLE traces (
    id                       BIGSERIAL PRIMARY KEY,
    message_id               BIGINT NOT NULL,
    reply_id                 BIGINT,
    chat_id                  TEXT NOT NULL,
    orchestrator_model       TEXT NOT NULL DEFAULT '',
    synthesizer_model        TEXT NOT NULL DEFAULT '',
    iterations               INT NOT NULL DEFAULT 0,
    summary                  TEXT,
    total_prompt_tokens      INT NOT NULL DEFAULT 0,
    total_completion_tokens  INT NOT NULL DEFAULT 0,
    synth_prompt_tokens      INT NOT NULL DEFAULT 0,
    synth_completion_tokens  INT NOT NULL DEFAULT 0,
    duration_ms              INT,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE tool_calls (
    id              BIGSERIAL PRIMARY KEY,
    trace_id        BIGINT NOT NULL REFERENCES traces(id) ON DELETE CASCADE,
    iteration       INT NOT NULL,
    seq             INT NOT NULL DEFAULT 0,
    tool_name       TEXT NOT NULL,
    arguments       TEXT NOT NULL DEFAULT '',
    normalized_args TEXT NOT NULL DEFAULT '',
    result          TEXT,
    is_error        BOOLEAN NOT NULL DEFAULT FALSE,
    duration_ms     INT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

IVFFlat cosine indexes on all embedding columns. Agent-created business
tables (e.g. `argus_food_log`) live alongside these and are created via
the `db` tool's `create` verb. The `argus_` prefix is applied transparently
by the tool layer.

### Database Sub-objects

The `Database` struct groups operations by feature, each holding a clone
of the underlying `PgPool` (zero-cost, `PgPool` is internally `Arc`):

```rust
pub(crate) struct Database {
    pub(crate) messages: Messages,
    pub(crate) notifications: Notifications,
    pub(crate) conversation: Conversation,
    pub(crate) documents: Documents,
    pub(crate) memories: Memories,
    pub(crate) traces: Traces,
}
```

### Write Safety (Idempotent Async Updates)

Background workers (embedding, summarization) write asynchronously.
All async UPDATE statements use precondition WHERE clauses:

| Operation | Precondition |
|-----------|-------------|
| `set_embedding` (messages, notifications, memories, chunks) | `WHERE embedding IS NULL` |
| `set_summary` (notifications) | `WHERE summary IS NULL` |
| `update_status` (documents) | State machine enforcement via valid_from_states |

Database is **required** — no fallback to in-memory store. PostgreSQL with
pgvector must be running. `docker-compose.yaml` provided for one-command
setup.

---

## Configuration

Single TOML config file. CLI flag: `--config <path>` (default
`~/.config/argus/argus.toml`). Workspace directory is configured in the
file (default `~/.local/share/argus`).

```toml
workspace_dir = "/data/argus"

[gateway.feishu]
app_id = ""
app_secret = ""
# base_url = "https://open.feishu.cn"  # default; use Lark URL for international

[gateway.feishu.transcription]
upstream = "local"
model_name = "whisper-large-v3"

[upstream.local]
type = "openai"
base_url = "http://localhost:8000/v1"
api_key = "omlx"
timeout_secs = 240

[upstream.anthropic]
type = "anthropic"
api_key = ""
# timeout_secs = 120  # default

[upstream.gemini]
type = "gemini"
api_key = ""

[agent]
max_iterations = 10
orchestrator_context_window = 10
tavily_api_key = ""

[agent.orchestrator]
upstream = "anthropic"
model_name = "claude-haiku-4-5"
max_tokens = 4096

[agent.synthesizer]
upstream = "gemini"
model_name = "gemini-2.5-flash-lite"
max_tokens = 32768

[database]
dsn = "postgres://argus:argus@localhost:5432/argus?sslmode=disable"

[embedder]
upstream = "local"
model_name = "modernbert-embed-base"
batch_size = 32
interval_secs = 30
summary_interval_ticks = 10  # summarize every 10 × interval_secs = 300s

[embedder.summarizer]
upstream = "gemini"
model_name = "gemini-2.5-flash-lite"
```

---

## Project Structure

```
Cargo.toml                   Workspace: root crate + feishu crate
feishu/                      Feishu SDK (workspace member)
  src/
    lib.rs                   Re-exports: Client, api, types, ws
    client.rs                Unified client: WS connect + REST API
    auth.rs                  Tenant access token (Mutex + Notify pattern)
    api.rs                   REST API: reply, update, upload image, download
    ws.rs                    WebSocket event stream + PBBP2 frame decoder
    pbbp2.rs                 Feishu binary protocol (protobuf)
    types.rs                 FeishuEvent, Error, API types
src/
  main.rs                    Entry point, --config flag, tokio::join! four services
  config.rs                  TOML config + tilde expansion + path resolution
  agent/
    mod.rs                   Message/Notification/Event types, Agent struct,
                             sync_message_loop + async_task_loop (join!),
                             run_orchestrator, run_synthesizer, run_task,
                             prompts, tool budgets
    harness.rs               Context curation: semantic recall + sliding window +
                             pinned memories
    skill.rs                 SkillIndex: load SKILL.md files at startup, catalog
    tool/
      mod.rs                 Tool trait, ToolRegistry, build_registry
      finish_task.rs         Sentinel tool (orchestrator exit signal)
      current_time.rs        Date/time with timezone
      search.rs              Web search (Tavily + DuckDuckGo fallback)
      fetch.rs               URL → readable text (HTML stripped via scraper)
      read_file.rs           Read file (workspace)
      write_file.rs          Write file (.users/ only)
      cli.rs                 Shell commands (host execution)
      db.rs                  Structured DB tool (7 verbs, typed casts)
      remember.rs            Pin a memory
      forget.rs              Deactivate a memory
      search_docs.rs         Semantic search over document chunks
      list_docs.rs           List indexed documents
      search_history.rs      Conversation history semantic search
      skill.rs               activate_skill tool
      create_task.rs         Async task creation tool
  gateway/
    mod.rs                   Gateway struct, Im trait, recovery routing
    feishu.rs                Feishu adapter: WS inbound, media processing,
                             card rendering, recovery handler
    transcribe.rs            Whisper transcription client (OpenAI-compatible)
  upstream/
    mod.rs                   Upstream registry, client_for factory
    types.rs                 Client trait, Message, ToolCall, Response,
                             StreamChunk, ChunkStream, errors (thiserror)
    openai.rs                OpenAI-compatible client (also used for Gemini)
    anthropic.rs             Anthropic Messages API client
  database/
    mod.rs                   Database struct, migration runner, sub-objects
    messages.rs              Messages table operations
    notifications.rs         Notifications table (agent replies)
    conversation.rs          Conversation view queries (recall + recent)
    documents.rs             Documents + chunks tables
    memories.rs              Memories table
    traces.rs                Traces + tool_calls tables, TraceBuilder
  embedder/
    mod.rs                   Embedder service: embed/summarize/ingest cycles
    client.rs                Embedding HTTP client (OpenAI-compatible)
    docindex.rs              Document ingestion: extract → chunk → save
  render.rs                  LaTeX detection + RaTeX rendering + Feishu card building
  recovery.rs                Recovery service: scan unreplied + replay
migrations/
  001_init.sql               messages table
  002_memory_system.sql      pgvector + memories + documents + chunks
  003_message_queue.sql      reply_status + trigger_msg_id
  004_traces.sql             traces + tool_calls tables
  005_message_summary.sql    summary column for assistant replies
  006_reply_content.sql      Split messages → messages + notifications
  007_conversation_view.sql  conversation view
```

---

## Tech Stack

| Component | Choice |
|-----------|--------|
| Language | Rust (edition 2024) |
| Async runtime | Tokio |
| IM | Feishu WebSocket (not webhook) + REST API |
| Feishu SDK | Workspace member crate (`feishu/`): WS via `tokio-tungstenite`, PBBP2 via `prost` |
| Chat Model | Multi-backend: OpenAI, Anthropic (native), Gemini (OpenAI-compatible) |
| Transcription | Whisper Large v3 via OpenAI-compatible `/v1/audio/transcriptions` |
| Embedding | modernbert-embed-base (768 dim) via OpenAI-compatible `/v1/embeddings` |
| Database | PostgreSQL + pgvector (required) |
| SQL | sqlx (compile-time-safe queries, PgPool) |
| HTTP | reqwest + reqwest-eventsource (SSE streaming) |
| Web Search | Tavily API (DuckDuckGo fallback when no API key) |
| HTML parsing | scraper crate |
| LaTeX | RaTeX (pure Rust crate, embedded fonts) → PNG |
| Output Format | Feishu interactive cards (schema 2.0, update_multi) |
| Streaming | SSE from model → Event::Reply → card update |
| Skills | SKILL.md files loaded at startup, activate_skill tool on demand |
| Config | TOML (via `toml` crate) |
| CLI | clap (derive) |
| Errors | thiserror (structured, per-module error enums) |
| Auth | Mutex + Notify lock-free-during-HTTP pattern (not RwLock) |
| Deployment | Single binary, `--config` flag, `docker-compose.yaml` for PostgreSQL |

---

## Deployment Constraints

- **Single instance only.** Task ordering relies on a single mpsc
  channel inside the Agent. The `FuturesUnordered` pool in the Feishu
  adapter assumes a single event loop. To support horizontal scaling,
  the task queue would need to move to a database-backed mechanism
  (e.g. `SKIP LOCKED`) and the WS connection would need leader election.

- **Host execution.** The `cli` tool runs commands directly on the host
  via `bash -c` or `sh -c`. No sandbox abstraction, no Docker isolation,
  no resource limits. Suitable for a trusted personal machine.

- **WebSocket, not webhook.** Feishu events arrive via a persistent
  WebSocket connection (Feishu's newer protocol), not via HTTP webhook
  callbacks. This eliminates the need for a public HTTP endpoint but
  requires reconnection handling (implemented with automatic retry
  and 5s backoff on transient failures, fatal error detection on
  auth failures).

---

## Future Work

- **Async task persistence and retry.** Currently v1: no persistence,
  no retry, tasks lost on restart. Future: persist TaskSpec to DB,
  resume on startup, retry on failure.

- **Async task user interaction.** Query task status, cancel running
  tasks, dedicated model config per task.

- **Per-chat FIFO within Agent.** Currently uses a single global
  sequential loop. Per-chat parallelism would allow cross-chat
  concurrency while preserving per-chat ordering.

- **Cron / scheduled tasks.** Cron-driven triggers that create async
  tasks on a schedule.

- **Sandbox abstraction.** The CLI tool runs commands directly on the
  host. Re-introduce sandbox abstraction (Docker, VM) if production
  deployment requires isolation.

- **Group Silent Listening.** Listed as an interaction mode but not
  implemented. Group messages without @mention are silently dropped.

- **Skill hot-reload.** Skills are loaded once at startup. Adding a
  file watcher or periodic rescan would restore hot-reload capability.

---

## Design Trade-offs (Deliberate Non-Goals)

Considered and rejected. Documenting the reasoning so future contributors
don't re-propose them without new evidence.

**Local-only model deployment.** The initial architecture used only local
models (Qwen 3.5 MoE on Mac Studio) to keep API costs at zero. In
practice, local models at the 30B parameter range lack the instruction-
following capability needed for reliable agent orchestration. Trace
analysis showed: Qwen confused skill names with tool names, hallucinated
column names in SQL, looped identical searches, and ignored system-level
constraints. Even with all harness guardrails, the failure rate was too
high for daily use. Commercial APIs (even the cheapest tier) eliminated
these issues immediately.

**Raw SQL tools.** The original design exposed raw SQL to the model via
`db` (read-only) and `db_exec` (write) tools with AST-based rewriting.
Abandoned because: (1) local LLMs consistently used wrong column names
and invented SQL syntax; (2) the security surface of SQL rewriting was
large; (3) raw SQL traces are hard to analyze programmatically. Replaced
with a single structured `db` tool (CLI+JSON syntax, 7 verbs).

**Semantic recall query rewriting.** A lightweight LLM call could rewrite
pronoun-heavy queries into self-contained search terms before pgvector
lookup. Rejected because each rewrite adds a full model round-trip to
every message. The sliding window provides pronoun context for most cases.

**Full assistant replies in orchestrator context.** Past long agent replies
in the sliding window dominated orchestrator attention. Solved: the
`conversation` module's `format_reply` uses notification summaries
(generated by the Embedder worker) for replies over 800 chars. Both
sliding window and semantic recall apply this truncation automatically.

**Tool output dynamic summarization.** Instead of byte-truncating tool
results at 16 KB, run a small model to summarize. Rejected for latency:
an extra LLM call per tool result adds 5-10s per iteration.

---

## Principles

- No "sessions", only a timeline
- Context is scarce — only high-signal tokens (Harness Engineering)
- **Trust no single model output** — harness enforces hard safety limits,
  not prompts
- Two narrow roles beat one wide role (Orchestrator + Synthesizer)
- **Ref-based async, not Arc/spawn** — all services borrow shared state
  via `&self` and run cooperatively in `tokio::join!`; no `Arc`, no
  `tokio::spawn`, no shared-nothing concurrency
- **Three layers, strict import direction** — Gateway → Agent → Upstream.
  Agent does not know which Gateway a message came from; Message carries
  its own `mpsc::Sender<Notification>` port. Layers communicate through
  value types (Message / Notification / Event / Payload), not service
  registries
- **Protect IM bytes, replay everything else** — the shutdown path
  guarantees media bytes land on disk. Content processing, agent
  execution, card rendering are all replayable via Recovery
- **Store first, process later** — persist the message before any
  processing or acknowledgment. Crash at any point = no data loss
- **Sequential sync, parallel async** — sync messages processed one
  at a time (history context consistency); async tasks run in parallel
  via `FuturesUnordered`. Both driven cooperatively by `join!`
- **High cohesion, low coupling** — each module owns its internal logic.
  Callers pass config sections, not pre-built internals. Internal types
  use `pub(crate)` or `pub(super)`, not `pub`. Trait objects at module
  boundaries, concrete types inside
- Skills grow organically through use, not through code changes
- All media saved to workspace for memory system reference
- **Use the best model for each role** — commercial APIs for orchestrator
  (tool calling quality) and synthesizer (answer quality); local models
  for transcription and embedding
- **Structured errors, no panics** — every module defines its own error
  enum via `thiserror`. No `unwrap()` on fallible operations, no `anyhow`.
  Error variants are exhaustive and machine-readable
