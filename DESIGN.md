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

---

## Architecture

### Three Decoupled Layers

Argus is structured as three layers with strict import boundaries:

```
┌────────────────────────────────┐
│  Frontend                      │  IM adapter, data prep, rendering
│  (Feishu; future CLI/Slack)    │
└────────────────────────────────┘
              │  SubmitTask
              ▼
┌────────────────────────────────┐
│  Agent                         │  task scheduler, two-phase execution,
│  (scheduler + HandleStream)    │  trace persistence
└────────────────────────────────┘
              │  uses
              ▼
┌────────────────────────────────┐
│  Backend                       │  LLM clients, tools, sandbox,
│  (model / tool / sandbox /     │  embedding, render, skills
│   embedding / render / skill)  │
└────────────────────────────────┘
```

Import direction is **Frontend → Agent → Backend**, never reversed.
The Agent does not `import` any Frontend implementation; each Task
carries its own Frontend reference, so UI callbacks route themselves.

The two inter-layer interfaces:

```go
type Agent interface {
    SubmitTask(Task)                   // mpsc entry; sync tasks only
    Run(ctx context.Context) error     // graceful start/stop
}

type Frontend interface {
    SubmitMessage(*Message)            // mpsc entry; rendering queue
    Run(ctx context.Context) error     // graceful start/stop
}
```

Async tasks (future: cron-driven, trace self-introspection, subagent
orchestration) are produced **inside** the Agent and never traverse
`SubmitTask`. The external surface stays minimal.

### Task, Message, Payload

```go
type Task struct {
    ChatID   string
    Frontend Frontend              // reply channel for this task's UI
    ReadyCh  <-chan Payload        // readiness signal + payload in one
    // ...metadata (db message id, kind, lang, ...)
}

type Payload struct {
    Content   string
    FilePaths []string
}

type Message struct {
    Events <-chan Event            // Agent produces, Frontend consumes
    // ...minimal metadata (chat id, db msg id)
}
```

`ReadyCh` is a buffered (cap 1) channel that carries the processed
payload directly — no DB round-trip between Frontend and Agent after
media is ready. `msg.Events` closing is the sole "this message is done"
signal; no explicit `Close()` method.

### Per-Task Lifecycle

```
Frontend inbound loop                 Agent run loop
─────────────────────                 ──────────────
webhook arrives                       task ← queue
  INSERT raw message                  msg := new Message
  Task{ReadyCh} constructed           task.Frontend.SubmitMessage(msg)
  agent.SubmitTask(task)              payload := <-task.ReadyCh
  go media.download+process           execute(payload, msg.Events)
                                      close(msg.Events)
media goroutine
──────────────                        Frontend render loop
download bytes → DB(downloaded)       ────────────────────
process → DB(processed)               msg ← outbound queue
payload → task.ReadyCh                open card
                                      for ev := range msg.Events:
                                          patch card
                                      close card
```

Agent calls `SubmitMessage` **before** waiting on `ReadyCh` — this
preserves the "thinking card appears instantly, even while audio is
transcribing" UX.

### Content State Machine

Two independent state columns on `messages`:

```
content_status:  raw → downloaded → processed
reply_status:    received → processing → done
```

- **raw**: webhook JSON stored; no bytes downloaded yet
- **downloaded**: media bytes saved to `.files/`; transcription/extraction
  pending. *Not interruptible* during graceful shutdown: Feishu file
  resources expire and re-download may fail.
- **processed**: transcription / OCR / post extraction done.
  *Reproducible from local bytes* — safe to re-run after crash or
  clean shutdown.

### Single Active Card Per Chat

This is a **Frontend implementation detail**, not an Agent scheduler
concern. The Feishu Frontend uses a per-chat mpsc render loop; at most
one card is "open" per chat at any instant. Agents may issue concurrent
`SubmitMessage` calls — the Frontend serializes rendering within each
chat because the IM UX requires it.

### Replay Subsystem (Crash Recovery)

Recovery is a standalone subsystem, not a Frontend feature. A
**Registry** indexed by `source_im` lets the replay subsystem route
each unfinished message back to the correct Frontend:

```
On startup:
  1. Each Frontend registers itself in Registry (by source_im tag)
  2. Replay subsystem scans messages WHERE reply_status != done
  3. For each row: lookup Registry[source_im] → construct Task → SubmitTask
  4. Agent processes replayed tasks identically to fresh ones
```

Replay handles the state combinations:
- `content_status=raw`: re-spawn download (may fail if Feishu resource expired — logged, moved to error)
- `content_status=downloaded`, not `processed`: re-run processing from local bytes
- `content_status=processed`, `reply_status != done`: direct SubmitTask

**Cards are not re-attached.** Feishu mostly rejects `update` calls on
older message IDs after a process restart. Replay opens a fresh card
per replayed task rather than relying on `reply_channel_id` continuity
(the old value is retained for audit only).

**Replay responsibility split**:
- Agent guarantees *every task is idempotent and fully replayable* from
  the DB state (key status transitions land on disk before the work starts)
- Replay subsystem decides *when* and *what* to resubmit
- Frontend provides the reply surface for replayed tasks via Registry

### "Don't Lose IM Bytes" Shutdown

One `ctx` cancel notifies everyone. Each loop decides how to exit
based on its own semantics; ordering falls out naturally from the
producer/consumer graph, no external staging controller.

**Inbound loop** (Frontend) on `ctx.Done`:
- close HTTP listener (no new webhooks accepted)
- drain inbound channel: remaining events get `INSERT` + `SubmitTask`
- exit

**Media goroutines** (Frontend) — *do not observe ctx*:
- each continues until its download phase lands bytes on disk
- processing phase (transcription / extraction) is replayable, so it
  may continue to completion or be abandoned at process exit; either
  way `Frontend.Run` waits on a `WaitGroup` tracking these goroutines
  before returning

**Agent run loop** on `ctx.Done`:
- stop taking new tasks from its queue
- **finish the task currently in hand** (user-experience priority: the
  Agent is slow relative to the Frontend, so the in-flight task is
  almost always what the user is watching). `msg.Events` gets closed
  normally at the end
- queued-but-not-started tasks are dropped; they are persisted and
  replayable on next startup
- exit

**Outbound (render) loop** (Frontend) on `ctx.Done`:
- drain the outbound queue: for each queued `msg`, consume
  `msg.Events` until closed, then close the card
- this naturally synchronizes with the Agent: the loop blocks on
  `msg.Events` exactly until the Agent's in-hand task closes it
- exit when the queue is empty

`Frontend.Run` returns after inbound + outbound + mediaWG all complete.
`Agent.Run` returns after its in-hand task completes.

```
ctx cancel
    │  (everyone sees it)
    ├── inbound loop:    drain inbound → exit
    ├── agent loop:      finish in-hand task (closes msg.Events) → exit
    ├── outbound loop:   drain outbound queue (events closed by the
    │                    exiting agent) → exit
    └── media goroutines: finish download + process → readyCh <- payload

Frontend.Run waits on (inbound + outbound + mediaWG), returns
Agent.Run   waits on (in-hand task), returns
```

**Principle**: only IM-provided bytes are protected by the shutdown
path; derived data relies on replay. A single ctx lets each loop
express its own "what must I finish before exiting" rule locally,
without a controller orchestrating stages.

### Per-Chat FIFO Within Agent

The per-chat MPSC channel structure that used to live in
`feishu/dispatcher.go` moves **into the Agent scheduler**. Semantics
are unchanged at this milestone:

- One consumer goroutine per chat (created lazily via `sync.Map.LoadOrStore`)
- Cross-chat parallelism preserved
- Sync tasks ordered per chat; async tasks (future work) will run on a
  global pool alongside this path, not replace it

### reply_channel_id Abstraction

The reply card handle remains IM-agnostic:
- **Feishu**: message_id from `ReplyRichWithID` (used for PATCH updates)
- **Slack** (future): message ts
- **CLI**: empty string

It lives in `messages.reply_channel_id`, written by the Frontend when
it opens a card. Agents never touch it; the audit role survives replay
even though the card itself does not.

### Two-Phase Agent (Orchestrator + Synthesizer)

A single-phase agent asks the model to do too much at once — understand the
request, choose tools, evaluate results, AND compose the answer. Weaker local
models skip tool calls entirely and answer from training memory. Splitting
into two narrow roles dramatically improves reliability:

- **Orchestrator** only calls tools. Its system prompt forbids text answers
  ("your text output is DISCARDED"). It loops until it calls `finish_task`
  with a summary, gets force-stopped by the harness (see below), or hits
  `max_iterations`.
- **Synthesizer** only writes the answer. It receives the user question, the
  curated history, and a `Materials` block containing every tool result and
  the orchestrator's summary. No tools are available — it must work from
  what Phase 1 gathered.

Both phases share the same conversation/material pipeline, but may use
different role-specific model clients (e.g. Claude for orchestration,
Gemini for synthesis). The difference is the system prompt, the tool list
(empty for Phase 2), and potentially the upstream provider.

### Tool vs Sandbox Orthogonality

These are unrelated axes:
- **Tool layer**: the LLM outputs tool calls — defines WHAT to do
- **Sandbox layer**: the sandbox executes — defines WHERE to run

Sandboxes are configurable:
- `local` — direct host execution via `bash -c` (development)
- `docker` — isolated container execution (production, image: `argus-sandbox`)

---

## Multimodal Input

Argus handles all Feishu message types natively:

| Message Type | Processing |
|-------------|-----------|
| **text** | Frontend inbound stores raw JSON → media goroutine extracts text (instant) |
| **image** | Frontend inbound stores raw JSON → media goroutine downloads to `.files/` + saves `file_paths` to DB |
| **post** (rich text) | Frontend inbound stores raw JSON → media goroutine extracts text + downloads images + saves `file_paths` to DB |
| **audio** | Frontend inbound stores raw JSON → media goroutine downloads `.opus` → Whisper → LLM correction |
| **file** (PDF, docx, etc.) | Frontend inbound stores raw JSON → media goroutine downloads to `.files/` → registers for RAG |

Media processing runs in async goroutines spawned by the Frontend
inbound loop. The inbound path is sub-millisecond: INSERT + construct
Task + `agent.SubmitTask` + spawn media goroutine. The media goroutine
sends the processed `Payload` through `task.ReadyCh` when complete.
The Agent opens the thinking card (via `task.Frontend.SubmitMessage`)
immediately on task pop, then blocks on `ReadyCh` until content is
ready.

### Vision (Multimodal Image Understanding)

Images flow end-to-end to vision-capable models (GPT-5.4, Gemini, Claude):

```
Feishu image/post → download to .files/ → file_paths saved to DB
    → Agent loads from disk via Payload.FilePaths → base64 data URL
    → NewMultimodalMessage(text, dataURLs...)
    → OpenAI: ContentPart array (native)
    → Anthropic: {type:"image", source:{type:"base64",...}}
    → Gemini: {inlineData:{mimeType, data}}
```

**Context budget**:
- Only the most recent image-bearing message in history gets real
  image data. Older image messages → text placeholder.
- Per-message hard limits: max 4 images, max 1 MB per image file.
  Excess images → `"[N image(s) omitted]"` text placeholder.
- Semantic recall returns text-only (no image re-injection); image
  messages that appear in both recall and sliding window stay in the
  sliding window path to preserve image data.

**Storage**: images are NOT stored as base64 in the DB (too heavy).
`messages.file_paths` stores the relative path to `.files/`; images
are loaded from disk on-demand when building model context.

### Audio Pipeline

```
Feishu audio → download → save .opus to .files/
    → Whisper v3 transcription (with domain vocabulary prompt)
    → confidence check (avg_logprob > -0.15 = high quality)
    → IF low confidence: LLM post-processing (punctuation, fix terms)
    → IF high confidence: skip LLM correction (saves ~5-10s)
    → text sent to orchestrator
```

The transcription prompt includes hints for:
- Mixed Chinese/English code-switching
- Technology terms (API, Kubernetes, Docker, LLM, MLX, omlx, vLLM)
- Finance terms (ETF, PE ratio, derivatives, hedge fund)
- Classical composers in Latin + Chinese (Chopin 肖邦, Scriabin 斯克里亚宾,
  Prokofiev 普罗科菲耶夫, Rachmaninoff 拉赫玛尼诺夫, …). Paired translit
  helps Whisper disambiguate proper names across languages.

Low-confidence transcriptions (avg_logprob < -0.15) are post-processed by
the orchestrator LLM to add punctuation and fix misheard terms. High-
confidence transcriptions skip this step (~5-10s savings). When correction
occurs, both raw and corrected text are logged for debugging.

### Document RAG (Personal Knowledge Base)

Non-image files (PDF, docx, etc.) go through the `docindex` ingester:
download → extract text via sandbox CLI (`pdftotext`, `pandoc`) → chunk →
embed each chunk → store in `chunks` table with pgvector index.

Retrieval happens through two complementary paths:

1. **Passive recall.** The harness's `curateHistory` embeds the current
   message and searches chunk embeddings via pgvector. Only chunks above
   a similarity threshold (0.35) are injected, with a total byte budget
   (4 KB) to prevent context pollution. When the knowledge base grows
   large, low-relevance chunks are silently dropped — the Orchestrator
   can always use `search_docs` for explicit retrieval.

2. **Active retrieval.** The Orchestrator has two tools for on-demand
   document access:
   - `list_docs` — lists all documents (ready, pending, processing, error)
     with chunk count and status. Shows errors for failed ingestions to
     help users troubleshoot.
   - `search_docs` — semantic search over chunks. Accepts a `query`,
     optional `limit` (default 5, max 20), and optional `filename` filter.
     The query is embedded via `EmbedOne`, then matched against chunk
     embeddings. When `filename` is specified, the tool over-fetches 5×
     and filters in Go (simpler than SQL-level filtering for small corpora).

The Orchestrator prompt instructs the model to use `list_docs` → `search_docs`
for user-uploaded documents, and explicitly prohibits reading binary files
via `read_file` or `cli`.

---

## Harness Engineering (Context Curation)

**Core principle: context is a scarce, finite resource. Only high-signal content goes in.**

The LLM never sees raw conversation history. Both phases go through a curation pipeline, but they build different system prompts:

```
Agent pops a task, opens card via Frontend, waits on ReadyCh for Payload
        ↓
[1] History Curation (shared by both phases)
    - Load recent N messages from store
    - Semantic recall: embed current message, pgvector search older messages
    - Filter: user messages + assistant final replies only
    - Remove: tool_call / tool_result noise
    - Re-inject images from .files/ for multi-turn vision
        ↓
[2a] Orchestrator Prompt                [2b] Synthesizer Prompt
     - OrchestratorPrompt (tool-only         - SynthesizerPrompt (answer-only
       rules, loop prevention)                 rules, language matching)
     - Environment (date, workspace)          - Environment
     - Pinned memories                        - Pinned memories
     - Builtin skill prompts                  (no skills/tools — just compose)
     - User skill catalog (name + desc)
        ↓                                        ↓
[3] Assemble                            [3] Assemble
    [sys] + [history] + [user]              [sys] + [history] + [user]
                                              + [materials from Phase 1]
        ↓                                        ↓
   Model.Chat (with tools)                  Model.ChatStream (no tools)
```

### Safety

- Tool results truncated to 16 KB max before entering context
- Messages saved BEFORE context assembly (prevents duplicates on retry/crash)
- Async embedding worker — saving is never blocked on the embed endpoint

---

## Tool Budgets & Loop Prevention

Prompt-layer rules are insufficient against weak-instruction-following
models. Argus enforces hard limits at the harness layer:

| Safeguard | Trigger | Action |
|-----------|---------|--------|
| **Per-tool budget** | `search`≤3, `fetch`≤4, `db`≤6, `cli`≤5, `write_file`≤3, `remember`≤3 per turn | Harness rejects further calls without dispatching; returns error "budget exhausted, call finish_task NOW" |
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
goroutines + `sync.WaitGroup`. Results are appended to the context in
the original order for deterministic history. `finish_task` is
pre-scanned before dispatch; budget checks run serially (shared state).

Latency: `max(tool_time)` instead of `Σ(tool_time)`.

### ChatWithEarlyAbort (Streaming Phase 1)

The orchestrator uses `ChatWithEarlyAbort` instead of blocking `Chat`.
This method opens an SSE stream and accumulates both `delta.content`
AND `delta.tool_calls` (OpenAI streaming format). Two exit paths:

- **Model calls tools**: tool_call deltas appear in the stream →
  accumulate until stream completes → return full Response with ToolCalls.
  Behaves identically to blocking Chat.
- **Model outputs text only** (ignored tool-only rule): content text
  exceeds 80 estimated tokens with no tool_call delta seen → cancel
  context immediately → return partial text → orchestrator retries.
  Saves 10-30s of wasted generation.

### Confidence-Based Transcription Skip

Whisper `avg_logprob > -0.15` indicates high-quality transcription. The
LLM correction step is skipped entirely, saving one model call (~5-10s)
per confident audio message. Threshold derived from production data:
`-0.104` (clear speech, skip) vs `-0.239` (noisy/proper-noun-heavy,
correct).

### Dynamic Streaming Throttle

Phase 2 card updates use a burst + throttle pattern:
- First 3 updates: sent immediately (user sees first tokens instantly)
- Subsequent updates: throttled to one per 500ms (Feishu rate-limit safe)

Perceived first-token latency drops from 500ms to ~0ms.

---

## Streaming

Phase 2 (synthesizer) streams its output via Server-Sent Events:

- `model.Client.ChatStream` returns `<-chan StreamChunk` with incremental
  deltas from an OpenAI-compatible streaming endpoint
- Agent emits `EventReplyDelta` with accumulated text on each chunk
- Feishu adapter uses burst + throttle (see above)
- During streaming, LaTeX rendering is skipped — partial `$...$` would
  break parsing. The final `EventReply` triggers full processing
  (LaTeX → PNG upload → `![](image_key)` embedding)

---

## Request Tracing

Every message processing is recorded in `traces` + `tool_calls` tables.
The Agent tees its own event stream — one path through `msg.Events` to
the Frontend (UI), one path to the trace collector (DB).

`traces`: one row per user-message → reply cycle. Fields: `message_id`,
`reply_id`, `chat_id`, `orchestrator_model`, `synthesizer_model`,
`iterations`, `summary`, prompt/completion tokens for both phases,
`duration_ms`.

`tool_calls`: one row per tool invocation. Fields: `trace_id`, `iteration`,
`seq`, `tool_name`, `arguments` (raw), `normalized_args` (parsed),
`result`, `is_error`, `duration_ms`.

### Normalized Args Schema

For the `db` tool, `normalized_args` is the deterministic output of
`Command.Normalize()` — a JSON object with stable key ordering:

```json
{
  "verb": "query",
  "table": "food_log",
  "where": [
    {"field": "meal_date", "op": "gte", "value": "2026-04-16"},
    {"field": "calories", "op": "gt", "value": 500}
  ],
  "sort": [{"field": "created_at", "order": "desc"}],
  "limit": 20
}
```

`where` is expanded from the `__` suffix syntax to explicit `{field, op,
value}` triples. This is the input format for future skill induction —
patterns like "user asks about food → verb=query, table=food_log,
where includes meal_date" can be extracted directly without re-parsing
the raw command string.

For non-db tools, `normalized_args` equals `arguments` (pass-through).

### Query defaults

`query` without `limit` defaults to 50 rows. Hard cap is 200 (enforced
by the tool regardless of what the model passes). `query` without `sort`
defaults to `created_at desc`.

---

## Model Strategy

### Why Commercial APIs

The original design used local models exclusively (Qwen 3.5 MoE on Mac
Studio). This worked for basic queries but had fundamental limitations:

- **Orchestrator quality**: local models (including Qwen 3.5 35B MoE)
  frequently misused tools — calling skill names as tool names, using
  wrong column names in SQL, looping the same search 20+ times, or
  ignoring tool-only instructions entirely. The harness budgets,
  early-abort, and retry mechanisms were all built to survive these
  failures. With commercial models (even Haiku-class), these harness
  mechanisms rarely fire — the model follows instructions correctly
  on the first attempt.
- **Synthesizer quality**: local models produced shallow, repetitive
  answers. Commercial models produce structured, insightful responses.
- **Cost reality**: at ~50-200 queries/day for a personal assistant,
  commercial API costs ($5-20/month) are negligible compared to the
  electricity cost of running a Mac Studio 24/7.

### Multi-Backend Architecture

Argus supports multiple model providers via named **upstreams**. Each
agent role (orchestrator, synthesizer, transcription) selects
its own upstream and model independently.

```yaml
upstreams:
  local:    {type: openai,    base_url: "http://localhost:8000/v1", api_key: "..."}
  openai:   {type: openai,    base_url: "https://api.openai.com/v1", api_key: "..."}
  anthropic:{type: anthropic,  api_key: "..."}
  gemini:   {type: gemini,     api_key: "..."}

model:
  orchestrator:  {upstream: openai,    model_name: "gpt-5.4"}
  synthesizer:   {upstream: gemini,    model_name: "gemini-2.5-flash-lite"}
  # No fallback — errors returned to user; message stays in queue for retry
  transcription: {upstream: local,     model_name: "whisper-large-v3"}
```

Four upstream types, all implemented with plain `net/http` (no SDKs):

| Type | Endpoint | Auth |
|------|----------|------|
| `openai` | Any OpenAI-compatible API | Bearer token |
| `anthropic` | Anthropic Messages API | `x-api-key` header |
| `gemini` | Google Gemini REST API | API key in URL |

**RetryClient** wraps each role's client. On 429 (rate limit), retries
up to 2× with 30s context-aware delay. Other errors are returned
directly to the user — no silent quality degradation. The user's
message stays in the DB queue and can be retried later.

### Recommended Configurations

| Orchestrator | Synthesizer | Cost | Quality |
|---|---|---|---|
| GPT-5.4 | Gemini 2.5 Flash Lite | ~$15/mo | Excellent tool calling + fast synthesis |
| Claude Haiku 4.5 | Gemini 2.5 Flash Lite | ~$5/mo | Good quality, best value |
| Qwen 3.5 (local) | Qwen 3.5 (local) | $0 | Functional but needs all harness guardrails |

### Local Models (Transcription + Embedding)

Local models serve two specialized roles:
- **Transcription**: Whisper Large v3 via omlx
- **Embedding**: modernbert-embed-base (768 dim) via omlx

KV cache 4-bit quantization recommended for local MoE models on
unified-memory Macs.

---

## Output Rendering

All agent output is delivered as **Feishu interactive cards** (`msg_type: "interactive"`), schema 2.0 with `update_multi: true` so the same card can be PATCHed multiple times as state evolves:

1. **Thinking card** — opened by the Frontend render loop on `SubmitMessage` pop ("💭 正在思考…")
2. **Tool status card** — humanized per tool (e.g. `🔍 正在搜索: X`)
3. **Composing card** — Phase 1→2 transition ("✍️ 正在撰写回复…")
4. **Streaming reply card** — first 3 updates instant, then ~500 ms throttle
5. **Final reply card** — full markdown + LaTeX images

All cards are PATCHed onto the same `reply_channel_id` created when the
Frontend opens the thinking card. The Frontend render loop consumes
`msg.Events` pushed by the Agent and drives the card lifecycle; closing
`msg.Events` is the signal to close (finalize) the card.

### LaTeX Rendering

Display LaTeX blocks (`$$…$$` or `\[…\]`) are detected in the final reply,
rendered to PNG via the embedded **RaTeX** renderer (Rust, via CGo),
uploaded to Feishu as images, and inline-replaced with
`[[IMG:image_key]]` markers — which the Feishu card's markdown block
renders as images.

---

## Skills

Skills follow the [Agent Skills open standard](https://agentskills.io) (same SKILL.md format as Claude Code).

### Two Types

**Builtin skills** — compiled into the binary, platform-conditional (`//go:build unix` / `windows`). Their full prompt is always injected into the orchestrator system prompt. Example: `posix-cli` teaches the model how to use grep, find, awk, sed, jq, pipelines, and enforces rules like "NEVER use `ls -R`, ALWAYS use `find`".

**User skills** — SKILL.md files in `workspace/.skills/`, authored by humans
(not the model). Loaded at startup, background-rescanned every 30 s. Only
name + description appear in the orchestrator's system prompt catalog; full
prompt loaded via `activate_skill` tool on demand. The model cannot create
or modify skills — `save_skill` has been removed from the tool registry.

User skills with the same name as a builtin override it.

### SKILL.md Format

```
workspace/.skills/
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

### Skill Lifecycle

Skills are authored and maintained by humans. The model can only read
them (via `activate_skill`), not create or modify them. Future: a
backend skill-induction system will analyze traces to propose new
skills, but the final authoring remains human-controlled.

---

## Tools

| Tool | Purpose | Budget |
|------|---------|--------|
| `read_file` | Read file contents (anywhere in workspace) | — |
| `write_file` | Write file (restricted to `.users/` directory) | 3/turn |
| `cli` | Execute shell commands via sandbox | 5/turn |
| `search` | Web search (Tavily API, DuckDuckGo fallback) | 3/turn |
| `fetch` | URL → readable text | 4/turn |
| `current_time` | Date/time with timezone support | — |
| `activate_skill` | Load a skill's full instructions on demand | — |
| `finish_task` | Sentinel — signals orchestrator → synthesizer transition | — |
| `db` | Structured data access (see below) | 6/turn |
| `remember` | Pin a persistent memory (pgvector indexed) | 3/turn |
| `forget` | Deactivate a pinned memory by ID | — |
| `search_docs` | Semantic search over indexed document chunks | — |
| `list_docs` | List all indexed documents in knowledge base | — |

Removed tools: `save_skill` (skills are human-authored), `db_exec` (replaced
by the structured `db` tool).

### Safety

- `read_file`: can read anywhere in workspace. Binary files (images,
  audio) return a description instead of raw bytes.
- `write_file`: **restricted to `.users/` directory**. The tool silently
  prepends `.users/` to any path. The model cannot overwrite skills
  (`.skills/`), media (`.files/`), or config files.
- `cli`: execution delegated to sandbox (local or Docker with resource limits).
- `db`: structured CLI+JSON interface — the model never writes SQL.
  See below.
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
correction: records are never removed, but `update` can modify field
values (e.g. correcting a calorie estimate). Every update sets
`updated_at` automatically, providing a basic audit trail. Full audit
history (storing every version of a row) is a future consideration.

**Namespace isolation**: the tool prepends `argus_` to all table names
internally. The model writes `food_log`; PG sees `argus_food_log`. System
tables (`messages`, `memories`, etc.) are unreachable because they don't
carry the prefix. No SQL parser needed — prefix is applied at the tool
layer before generating parameterized queries.

**Column validation**: before execution, the tool checks all referenced
column names against `information_schema.columns`. Unknown columns trigger
a Levenshtein fuzzy match suggestion: `"calory" → did you mean "calories"?`

**Query results**: returned as JSON arrays (preserving types), not
tab-separated text. `create` is idempotent (IF NOT EXISTS + returns
current schema). `update` only works by ID (no WHERE conditions).

**Types**: `text`, `number` (DOUBLE PRECISION), `date`, `boolean`,
`timestamp`, `json` (JSONB). Every table auto-gets `id` + `created_at` +
`updated_at`.

**Where operators**: exact match (default), `__gt`, `__gte`, `__lt`,
`__lte`, `__contains` (ILIKE), `__neq`.

---

## Media Storage

All downloaded media is saved to `workspace/.files/`:

```
workspace/.files/
  img_v3_xxx.png          # Feishu images
  file_v3_xxx.opus        # Voice messages
  file_v3_xxx.pdf         # Uploaded files → ingested into pgvector
```

Files are named by their Feishu file key + original extension. This directory serves as:
- Source for multi-turn image re-injection into context
- Source for document RAG ingestion
- Archive for the memory system to reference

---

## Memory

Argus has three memory layers, all active:

| Layer | Scope | Mechanism |
|-------|-------|-----------|
| **Sliding window** | Last `context_window` messages | Direct load from `messages` table |
| **Semantic recall** | All historical messages | pgvector cosine search on the current message's embedding, deduped against the window |
| **Pinned memories** | User-curated persistent notes | `memories` table; agent uses `remember`/`forget` tools; pgvector embeds them for recall |

### Startup Repair

On server startup, the `RepairableStore` interface runs:
- `RepairStuckDocuments` — mark stuck-in-processing docs as failed
- `RepairOrphanChunks` — clean chunks pointing at missing documents
- `CountUnembeddedMessages` — warn if embedding backlog
- `FailedTranscriptions` — warn on persistent audio failures

---

## Data Model (PostgreSQL + pgvector)

```sql
-- Core conversation history
CREATE TABLE messages (
    id          BIGSERIAL PRIMARY KEY,
    chat_id     TEXT NOT NULL,
    role        TEXT NOT NULL,
    content     TEXT NOT NULL,
    tool_name   TEXT,
    tool_call_id TEXT,
    -- metadata
    source_im   TEXT NOT NULL DEFAULT 'unknown',  -- feishu / cli / ...
    channel     TEXT NOT NULL DEFAULT '',
    source_ts   TIMESTAMPTZ,
    msg_type    TEXT NOT NULL DEFAULT 'text',     -- text / image / audio / file
    file_paths  TEXT[],
    sender_id   TEXT,
    embedding   vector(768),                      -- nullable; async-filled
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Pipeline state (user messages only; NULL for assistant/tool msgs)
    content_status   TEXT,                        -- raw/downloaded/processed
    reply_status     TEXT,                        -- received/processing/done
    reply_channel_id TEXT,                        -- IM-abstract card handle (Frontend-written)
    trigger_msg_id   TEXT                         -- IM trigger message ID (reply thread root)
);

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
    status     TEXT NOT NULL DEFAULT 'pending',  -- pending / processing / done / failed
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
```

IVFFlat cosine indexes on all three embedding columns. Agent-created
business tables (e.g. `argus_food_log`) live alongside these and are
created via the structured `db` tool's `create` verb. The `argus_` prefix
is applied transparently by the tool layer.

Database is optional — server mode falls back to memory store if PostgreSQL is unavailable, though semantic recall, pinned memories, and document RAG are disabled in that mode.

`docker-compose.yaml` provided for one-command PostgreSQL+pgvector setup: `make up`.

---

## Configuration

Single config file at `<workspace>/config.yaml`. CLI flag: `--workspace <dir>`.

```yaml
server:
  port: "8088"

feishu:
  app_id: ""
  app_secret: ""
  verification_token: ""
  encrypt_key: ""

# Named upstream providers. Supported types: openai, anthropic, gemini.
upstreams:
  local:
    type: openai
    base_url: "http://localhost:8000/v1"
    api_key: "omlx"
    timeout: 240s
  openai:
    type: openai
    base_url: "https://api.openai.com/v1"
    api_key: ""
    timeout: 120s
  anthropic:
    type: anthropic
    api_key: ""
    timeout: 120s
  gemini:
    type: gemini
    api_key: ""
    timeout: 120s

# Each agent role selects its own upstream + model.
# No fallback — if the API fails, the user is notified.
model:
  orchestrator:
    upstream: anthropic
    model_name: "claude-haiku-4-5"
    max_tokens: 4096
  synthesizer:
    upstream: gemini
    model_name: "gemini-2.5-flash-lite"
    max_tokens: 32768
  transcription:
    upstream: local
    model_name: "whisper-large-v3"

database:
  dsn: "postgres://argus:argus@localhost:5432/argus?sslmode=disable"

agent:
  max_iterations: 10
  context_window: 20                  # synthesizer history window
  orchestrator_context_window: 10     # smaller to save tokens
  skill_rescan: 30s

search:
  tavily_api_key: ""  # optional, falls back to DuckDuckGo
  include_answer: true
  max_results: 5

embedding:
  enabled: true
  upstream: local                     # uses its own upstream
  model_name: "modernbert-embed-base"
  batch_size: 32
  interval: 2s

sandbox:
  type: local             # "local" for dev, "docker" for production
  image: argus-sandbox
  timeout: 30s
  memory_limit: "512m"    # docker only
  network: "none"         # docker only
```

The legacy single-model format (`model.base_url`, `model.model_name`, etc.)
is still accepted via migration in `config.Load()` for backward compatibility,
but new deployments should use the `upstreams` + per-role `model` structure.

---

## Project Structure

```
cmd/argus/main.go            Entry point, --workspace flag (default ~/.argus)
internal/
  agent/
    agent.go                 Interface: SubmitTask + Run; Task/Payload/Message
                             types. Two-phase orchestrator + streaming synthesizer.
    scheduler.go             Per-chat MPSC channel, sync.Map chat registry,
                             graceful shutdown (drain current in-hand task)
    task_store.go            reply_status state transitions (processing → done)
                             wrapping messages table semantics
    harness.go               Context curation: history + two prompt builders
    prompts.go               OrchestratorPrompt + SynthesizerPrompt constants
    event.go                 Event types: Thinking, ToolCall, ToolResult,
                             ReplyDelta, Reply, Error
  frontend/
    frontend.go              Frontend interface (SubmitMessage / Run /
                             StopInbound); Registry indexed by source_im
  replay/
    replay.go                Startup replay subsystem: scan messages
                             WHERE reply_status != done; route by
                             source_im → Registry → construct Task →
                             agent.SubmitTask
  config/config.go           YAML config + env overrides
  docindex/ingest.go         Document RAG ingester (chunk + embed)
  embedding/
    client.go                Embedding HTTP client (OpenAI-compatible)
    worker.go                Async worker: embed unembedded messages/memories/chunks
  feishu/                    Frontend implementation for Feishu
    frontend.go              Implements frontend.Frontend; owns inbound +
                             outbound mpsc loops, per-chat render serialization
    handler.go               HTTP webhook handler: parse → inbound channel
    inbound.go               Inbound loop: INSERT raw → construct Task →
                             SubmitTask → spawn media goroutines
    media.go                 Download (protected) + process (replayable):
                             Whisper transcription, LLM correction, post extract
    render.go                Render loop: open card → consume msg.Events →
                             patch card → close card
    client.go                Feishu API: reply, send, upload image, download
    card.go                  Interactive card builders + per-tool humanizer
    event.go                 Event type definitions
    dedup.go                 Event ID deduplication
  model/
    model.go                 Client interface (Chat, ChatStream,
                             ChatWithEarlyAbort, Transcribe)
    openai.go                OpenAI-compatible (local + api.openai.com)
    anthropic.go             Anthropic Messages API (Claude)
    gemini.go                Google Gemini REST API
    fallback.go              RetryClient wrapper (429 retry, no fallback)
    factory.go               NewClientFromConfig / NewClientsForAgent
    types.go                 Message, ToolCall, Response, Usage
  render/
    renderer.go              Processor: markdown → LaTeX images → markers
    latex.go                 LaTeX detection + RaTeX PNG rendering
  sandbox/
    sandbox.go               Sandbox interface: Exec(ctx, command, workDir)
    local.go                 Host execution (bash -c)
    docker.go                Container execution (docker run)
  # sqlsandbox/ — removed (replaced by structured db tool)
  skill/
    index.go                 SkillIndex: thread-safe catalog + prompt lookup
    loader.go                FileLoader: load builtins + rescan .skills/
    builtin.go               Builtin dispatcher
    builtin_unix.go          POSIX CLI skill (grep/find/awk/sed/jq)
    builtin_windows.go       PowerShell CLI skill
  store/
    store.go                 Interface + sub-interfaces (Semantic, Pinned,
                             Document, QueueStore)
    postgres.go              PostgreSQL + pgvector + QueueStore + migrations
    memory.go                In-memory store (CLI / no-DB mode)
    repair.go                Startup repair helpers
    migrations/
      001_init.sql           messages table
      002_memory_system.sql  pgvector + memories + documents + chunks
      003_message_queue.sql  reply_status + reply_channel_id + trigger_msg_id
  tool/
    tool.go                  Tool interface + ToolDef conversion
    registry.go              Registry with name lookup
    file.go                  read_file (workspace) + write_file (.users/ only)
    cli.go                   Shell commands (delegates to sandbox)
    search.go                Web search (Tavily + DuckDuckGo fallback)
    fetch.go                 URL fetcher with HTML-to-text conversion
    current_time.go          Date/time with timezone support
    db_structured.go         Structured db tool (single CLI+JSON interface)
    db_command.go            Parser, executor, validator, normalizer
    activate_skill.go        Load skill prompt on demand
    remember.go / forget.go  Pinned memory management
    search_docs.go           Document search + list tools (knowledge base)
    finish_task.go           Sentinel tool (orchestrator exit signal)
    context.go               ChatID context key for tools
third_party/ratex/           Embedded Rust LaTeX renderer (CGo)
```

---

## Tech Stack

| Component | Choice |
|-----------|--------|
| Language | Go |
| IM | Feishu Bot API (text, image, audio, file, rich text, interactive cards) |
| Chat Model | Multi-backend: OpenAI (GPT-5.4), Anthropic (Claude), Gemini |
| Transcription | Whisper Large v3 via `/v1/audio/transcriptions` |
| Embedding | modernbert-embed-base (768 dim) via `/v1/embeddings` |
| Database | PostgreSQL + pgvector (optional, memory store fallback) |
| Sandbox | Local (dev) / Docker (prod) |
| Web Search | Tavily API (DuckDuckGo fallback when no API key) |
| LaTeX | RaTeX (Rust, CGo-embedded) → PNG → Feishu image |
| Output Format | Feishu interactive cards (schema 2.0, update_multi) |
| Streaming | SSE from model → burst first 3 tokens + throttled 500ms |
| Skills | SKILL.md files (Agent Skills standard) + compiled builtins |
| Deployment | Single binary, `--workspace` flag, `docker-compose.yaml` for PostgreSQL |

---

## Deployment Constraints

- **Single instance only.** Per-chat serialization (inside Agent
  scheduler and Frontend render loop) relies on in-memory `sync.Map`
  + goroutine-per-chat. ReadyCh coordination between Frontend media
  goroutines and Agent task execution assumes a single server process.
  To support horizontal scaling, the "one consumer per chat" guarantee
  at both layers would need to be promoted to DB advisory locks or
  leases, and the ReadyCh channel replaced with a DB-poll or pub/sub
  mechanism that can cross process boundaries.

- **Webhook security.** The Handler currently parses the Feishu webhook
  envelope and deduplicates by event ID, but does not verify the
  `verification_token` or decrypt `encrypt_key`. Suitable for trusted
  networks / development. Before exposing to the public internet,
  signature verification must be implemented as a blocking requirement.

---

## Future Work

- **Group Silent Listening.** DESIGN.md lists this as an interaction mode
  but it is not implemented — group messages without @mention are silently
  dropped. Future implementation needs to decide: should silent messages
  enter the same timeline? Be embedded for semantic recall? How to handle
  privacy / group-chat permission boundaries?

- ~~**Document RAG retrieval.**~~ Implemented: `search_docs` + `list_docs`
  tools for active retrieval, plus passive chunk recall in `curateHistory`.

- ~~**Multi-modal image re-injection.**~~ Implemented: `StoredMessage.FilePaths`
  populated by `ProcessMedia`, `curateHistory` re-injects from that field.

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
constraints. Even with all harness guardrails (budgets, early-abort,
retry, prompt engineering), the failure rate was too high for daily use.
Switching to commercial APIs (even the cheapest tier — Claude Haiku)
eliminated these issues immediately. Local models are retained as
fallback and for transcription/embedding where they perform well.

**Raw SQL tools (db / db\_exec).** The original design exposed raw SQL to
the model via `db` (read-only) and `db_exec` (write) tools, with an
AST-based `sqlsandbox` rewriter (libpg_query) to enforce table-name
prefixing. This was abandoned for three reasons:

1. *Model unreliability.* Local LLMs (Gemma 4, Qwen 3.5) consistently
   used wrong column names (`date` vs `meal_date`), invented SQL
   syntax, and looped on failed queries with trivial variations. The
   schema-hint-on-error mechanism mitigated but didn't solve this.
2. *Security surface.* Even with AST rewriting, SQL identifiers (table
   names, column names) were spliced into queries. The rewriter handled
   known patterns (RangeVar, IndexStmt, DropStmt) but the attack
   surface was large — every new SQL feature required new AST handlers.
3. *Trace opacity.* Raw SQL strings are hard to analyze programmatically.
   Building a skill-induction system on top of SQL traces would require
   another SQL parser, re-introducing the same complexity we wanted to
   avoid.

Replaced with a single structured `db` tool (CLI+JSON syntax, 7 verbs).
The model describes intent (`query food_log where {"meal_date": "..."}`);
the tool generates parameterized SQL internally. Identifiers are validated
against a strict regex + double-quoted in SQL. The `sqlsandbox` package
and `pg_query_go` dependency were deleted entirely.

**Semantic recall query rewriting.** A lightweight LLM call could rewrite
pronoun-heavy queries ("那个文件", "它") into self-contained search terms
before the pgvector lookup. Rejected because each rewrite adds a full
model round-trip (5-10s on local MoE) to every message — the latency cost
exceeds the retrieval quality gain. The sliding window of recent messages
already provides pronoun context for most practical cases. Revisit if a
sub-100ms rewrite model becomes available locally.

**Tool output dynamic summarization.** Instead of byte-truncating tool
results at 16 KB, run a small model to summarize long outputs (web pages,
large query results). Rejected for the same latency reason: an extra LLM
call per tool result would add 5-10s per orchestrator iteration. The
current truncation + schema-hint-on-error approach handles the common
cases (wrong column name, empty result) without an extra model call.
The 16 KB limit is large enough for most structured data; for web pages,
the `fetch` tool already strips HTML to readable text before truncation.

**Media download breakpoint resume.** Feishu media files are typically
small (audio: 3-30 KB opus, images: 50-200 KB PNG). Download completes
in under a second even on slow connections. Implementing HTTP range
requests, partial-download state tracking, and resume logic would add
significant complexity for negligible benefit. If Argus later supports
an IM that transfers large files (video, multi-MB documents), this
decision should be revisited.

---

## Principles

- No "sessions", only a timeline
- Context is scarce — only high-signal tokens (Harness Engineering)
- **Trust no single model output** — harness enforces hard safety limits,
  not prompts
- Two narrow roles beat one wide role (Orchestrator + Synthesizer)
- Tool calls and execution are orthogonal (tool layer × sandbox layer)
- **Three layers, strict import direction** — Frontend → Agent → Backend.
  Agent does not know which Frontend a task came from; Task carries its
  own Frontend reference. Layers communicate through value types
  (Task / Message / Event / Payload), not service registries
- **Protect IM bytes, replay everything else** — the shutdown path
  guarantees webhook bytes + downloaded media land on disk. Transcription,
  extraction, agent execution, card rendering are all re-playable
- **Finish the task currently in hand** at shutdown — Agent is slower
  than Frontend, so the in-flight task is almost always the user's
  visible thinking card. Completing it preserves UX; dropped queue
  items are recovered by replay
- **Single active card per chat is a Frontend concern** — the Agent
  may be concurrent across chats; rendering serialization is implemented
  in the Frontend's per-chat render loop because the IM UX requires it
- **Store first, process later** — persist the message before any
  processing or acknowledgment. Crash at any point = no data loss
- **Async tasks are internal** — `SubmitTask` accepts only synchronous,
  IM-originated tasks. Cron-driven, self-introspection, and subagent
  tasks are produced inside the Agent and never traverse the external
  interface; this keeps the surface minimal and future-proof
- Builtin skills compiled in with platform build tags; user skills are files
- Skills grow organically through use, not through code changes
- All media saved to workspace for memory system reference
- **Use the best model for each role** — commercial APIs for orchestrator
  (tool calling quality) and synthesizer (answer quality); local models
  for fallback, transcription, and embedding. The harness guardrails
  (budgets, early-abort, retry) remain as safety nets but should rarely
  fire with capable models
