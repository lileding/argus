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

### Handler + Dispatcher (Channel-per-Chat)

Message processing has two components. The `messages.reply_status` column
is the DB-side state machine:

```
received → ready → processing → done
```

```
IM message arrives
    ↓
HANDLER (fully reentrant, sub-millisecond)
  ├─ parse webhook
  ├─ INSERT raw message JSON → status=received
  ├─ push QueuedMessage{ReadyCh} to Dispatcher's per-chat channel
  └─ go ProcessMedia(msg, readyCh)
       ├─ download media / transcribe audio / LLM correction
       ├─ UPDATE content → processed text
       ├─ UPDATE status → ready
       └─ close(readyCh)
    ↓
DISPATCHER (one goroutine per chat, MPSC channel)
  ├─ pop from channel
  ├─ open thinking card IMMEDIATELY (one card per chat)
  ├─ <-msg.ReadyCh  ← blocks until media goroutine finishes
  │     (text = instant, audio = ~5s while card shows "thinking")
  ├─ ClaimNextReply from DB (ready → processing)
  ├─ Orchestrator (Phase 1): tools only, hard budgets
  ├─ Synthesizer (Phase 2): SSE streaming → throttled card updates
  ├─ FinishReply (processing → done)
  └─ loop: pop next message from channel
```

There is no separate Filter stage. Media processing is an async goroutine
per message, synchronized with the Dispatcher via a `chan struct{}`
(`ReadyCh`). This solves the card-timing problem:

- **Card opens immediately**: Dispatcher pops → opens card before
  waiting on ReadyCh. The user sees "💭 thinking" the instant the
  Dispatcher reaches their message.
- **At most one card per chat**: Dispatcher processes one message at a
  time per chat. The next message's card only appears after the
  previous reply is sent.
- **No wasted wait for text messages**: ProcessMedia for text is just
  a JSON parse — ReadyCh is closed before the Dispatcher even reads it.

### Per-Chat FIFO Serialization

Each chat gets a buffered `chan QueuedMessage` (MPSC: Handler pushes,
Dispatcher's goroutine consumes). Created lazily on first push via
`sync.Map.LoadOrStore`. Different chats run fully in parallel.

The Handler's inbound path is completely reentrant — 5 messages arriving
simultaneously for the same chat each do INSERT + channel push in
sub-millisecond time. The Dispatcher drains them strictly FIFO.

### Crash Recovery

On startup, `Dispatcher.Recover`:
1. `processing → ready` in DB (agent was mid-run → re-process)
2. `received` messages → re-spawn `ProcessMedia` goroutines
   (media download was interrupted → retry from scratch)
3. `ready` messages → push to chat channels with pre-closed ReadyCh

DB `source_im` + `msg_type` tell the recovery code which media
processor to spawn (currently only Feishu; future IMs register their
own processors).

### reply_channel_id Abstraction

The reply card handle is IM-agnostic:
- **Feishu**: message_id from ReplyRichWithID (used for PATCH updates)
- **Slack** (future): message ts
- **CLI**: empty string

The Adapter uses this handle to drive the card lifecycle without knowing
which IM created it.

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

### Async Task Architecture (Planned)

The next architecture step is to separate immediate conversation turns
from durable background work. The system becomes three layers:

- **IM adapter layer**: Feishu/Slack/CLI adapters parse inbound messages,
  download media, prepare normalized inputs, and render outbound events.
  They do not decide how long-running work is scheduled.
- **Agent engine layer**: the task scheduler, task workers, two-phase
  Agent, tool registry, traces, and presentation queue live here. This
  layer decides whether work runs synchronously or asynchronously.
- **Model backend layer**: OpenAI/Anthropic/Gemini/local adapters expose
  uniform chat, streaming, transcription, and embedding APIs. They know
  nothing about IM cards, cron schedules, or task locking.

Two task classes share the same Agent and tool infrastructure:

- **Sync tasks** are user-visible conversation turns. They keep the
  existing per-chat FIFO behavior: one active sync reply card per chat,
  streaming updates, and immediate final answer.
- **Async tasks** are durable background jobs. They can be triggered by
  cron schedules or created by a sync task when the orchestrator detects
  long-running work (code changes, large document processing, multi-step
  research, or an explicit "run this in the background" request).

Async task execution is parallel. Presentation is not. Completed async
tasks write an outbound notification event, and a per-chat presenter
serializes delivery so it never conflicts with an active sync reply card.
This preserves the current chat UX while allowing background work to use
all available worker capacity.

#### Task State Machine

Async tasks must be persisted before execution. The worker pool claims
queued tasks with a lease, runs them, records the result, and emits an
outbound event for delivery.

```
queued → running → succeeded
                 → failed
                 → cancelled
```

`running` tasks carry `lease_owner` and `lease_until`. On startup, tasks
whose lease expired are moved back to `queued` unless they are already in
a terminal state. This gives async jobs the same "store first, process
later" recovery property as IM messages.

Planned table:

```sql
CREATE TABLE tasks (
    id                 BIGSERIAL PRIMARY KEY,
    kind               TEXT NOT NULL, -- sync / async
    source             TEXT NOT NULL, -- im / cron / agent
    chat_id            TEXT NOT NULL,
    user_id            TEXT,
    parent_task_id     BIGINT REFERENCES tasks(id),
    trigger_message_id BIGINT REFERENCES messages(id),
    status             TEXT NOT NULL DEFAULT 'queued',
    priority           INT NOT NULL DEFAULT 0,
    title              TEXT NOT NULL DEFAULT '',
    input              JSONB NOT NULL DEFAULT '{}'::jsonb,
    result             TEXT NOT NULL DEFAULT '',
    error              TEXT NOT NULL DEFAULT '',
    lease_owner        TEXT,
    lease_until        TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at         TIMESTAMPTZ,
    finished_at        TIMESTAMPTZ
);
```

`messages` remains the conversation timeline and context source. `tasks`
is the execution ledger. A sync IM turn can still be represented by the
existing `messages.reply_status` queue during the first migration stage,
but new async work should use `tasks` from day one so the two concepts do
not collapse into one overloaded table.

#### Outbox and Presentation Lock

Async workers never call IM APIs directly. They append delivery requests
to an `outbox_events` table. A per-chat presenter consumes those events
and owns all card/message delivery for that chat.

```sql
CREATE TABLE outbox_events (
    id          BIGSERIAL PRIMARY KEY,
    chat_id     TEXT NOT NULL,
    task_id     BIGINT REFERENCES tasks(id),
    kind        TEXT NOT NULL, -- async_done / async_failed / reminder / notice
    payload     JSONB NOT NULL DEFAULT '{}'::jsonb,
    status      TEXT NOT NULL DEFAULT 'pending',
    priority    INT NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at     TIMESTAMPTZ
);
```

The presenter enforces a single rule: a chat can have only one active
presentation owner. While a sync task is streaming or patching its reply
card, async completion events for the same chat remain pending. When the
sync card reaches its final update, the presenter drains pending outbox
events in priority/created order.

This keeps two concerns separate:

- **Execution concurrency**: async tasks can run in parallel across all
  chats and can continue while a sync task is active.
- **User-visible ordering**: the chat receives one coherent sequence of
  cards and messages, with no async completion overwriting or interleaving
  with an unfinished sync reply card.

The first implementation can keep the presentation lock in memory because
Argus is currently single-instance. If horizontal scaling is introduced,
the lock must move to a DB lease or advisory lock, matching the existing
deployment constraint for per-chat FIFO.

#### Cron as Task Producer

Cron schedules are persisted configuration, not execution. A scheduler
tick finds due schedules, creates async tasks, advances `next_run_at`,
and exits. The async task worker then runs the Agent and uses the outbox
for delivery.

```sql
CREATE TABLE cron_schedules (
    id                 BIGSERIAL PRIMARY KEY,
    chat_id            TEXT NOT NULL,
    user_id            TEXT,
    name               TEXT NOT NULL,
    schedule_type      TEXT NOT NULL DEFAULT 'daily',
    cron_expr          TEXT,
    hour               INT,
    minute             INT,
    timezone           TEXT NOT NULL DEFAULT 'Asia/Shanghai',
    prompt             TEXT NOT NULL,
    enabled            BOOLEAN NOT NULL DEFAULT TRUE,
    created_by_task_id BIGINT REFERENCES tasks(id),
    last_run_at        TIMESTAMPTZ,
    next_run_at        TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

The initial scheduling scope should be intentionally small: daily jobs
with explicit hour, minute, and timezone. Natural-language requests such
as "每天晚上10点做当日饮食统计" should be converted by the orchestrator
into a `create_cron` tool call that stores a daily schedule. Weekly,
monthly, and full cron expression support can be added after daily
schedules are reliable and visible to the user.

This replaces the current config-only cron model. Static jobs in
`config.yaml` can remain as a bootstrap path, but runtime user-created
schedules must live in the database so they can be listed, edited,
disabled, and recovered after restart.

#### Agent-Facing Task Tools

The orchestrator should manage async work through tools, not hidden
prompt conventions:

| Tool | Purpose |
|------|---------|
| `create_async_task` | Create a durable background task from the current sync turn |
| `get_task_status` | Inspect queued/running/completed task state |
| `cancel_task` | Mark a queued/running task as cancelled when possible |
| `create_cron` | Store a user-visible schedule that creates async tasks |
| `list_cron` | Show active and disabled schedules for the current user/chat |
| `delete_cron` | Disable a schedule |

The `create_async_task` tool should be conservative. It is appropriate
for long-running code work, large document processing, multi-step
research, explicit background requests, or work that needs to outlive the
current sync reply. It should not be used for ordinary short questions,
simple searches, or small structured data queries.

Implementation order:

1. ~~Add `tasks`, async worker leases, worker execution, and basic task tools.~~
2. ~~Add `outbox_events` and per-chat presentation serialization.~~
3. Link traces to `task_id` / `parent_task_id`.
4. Add `cron_schedules` and daily schedule execution.
5. Add `create_cron`, `list_cron`, and `delete_cron`.
6. Migrate config-only cron jobs into bootstrap schedules or deprecate
   them once DB-backed schedules are stable.

---

## Multimodal Input

Argus handles all Feishu message types natively:

| Message Type | Processing |
|-------------|-----------|
| **text** | Handler stores raw JSON → `ProcessMedia` extracts text (instant) |
| **image** | Handler stores raw JSON → `ProcessMedia` downloads to `.files/` + saves `file_paths` to DB |
| **post** (rich text) | Handler stores raw JSON → `ProcessMedia` extracts text + downloads images + saves `file_paths` to DB |
| **audio** | Handler stores raw JSON → `ProcessMedia` downloads `.opus` → Whisper → LLM correction |
| **file** (PDF, docx, etc.) | Handler stores raw JSON → `ProcessMedia` downloads to `.files/` → registers for RAG |

All media processing runs in an async goroutine (`ProcessMedia`) spawned
by the Handler. The Handler's inbound path is sub-millisecond: INSERT +
channel push + goroutine spawn. The Dispatcher opens the thinking card
immediately on pop, then blocks on the goroutine's `ReadyCh` until
content is ready.

### Vision (Multimodal Image Understanding)

Images flow end-to-end to vision-capable models (GPT-5.4, Gemini, Claude):

```
Feishu image/post → download to .files/ → file_paths saved to DB
    → HandleStreamQueued loads from disk → base64 data URL
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
Dispatcher pops a message, waits on ReadyCh, claims from DB
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
The Dispatcher tees the agent event stream — one path to the Adapter
(UI), one path to the trace collector (DB).

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

1. **Thinking card** — sent by Dispatcher on pop ("💭 正在思考…")
2. **Tool status card** — humanized per tool (e.g. `🔍 正在搜索: X`)
3. **Composing card** — Phase 1→2 transition ("✍️ 正在撰写回复…")
4. **Streaming reply card** — first 3 updates instant, then ~500 ms throttle
5. **Final reply card** — full markdown + LaTeX images

All cards are PATCHed onto the same `reply_channel_id` created by the
thinking card. The adapter receives events from the Dispatcher (which
runs the agent) and drives the card lifecycle.

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
| `create_async_task` | Create a durable background task | 2/turn |
| `get_task_status` | Inspect background task state | — |
| `cancel_task` | Cancel a queued/running background task | — |
| `create_cron` | Create a DB-backed schedule that emits async tasks (planned) | — |
| `list_cron` | List schedules for the current user/chat (planned) | — |
| `delete_cron` | Disable a schedule (planned) | — |

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
    source_im   TEXT NOT NULL DEFAULT 'unknown',  -- feishu / cli / cron
    channel     TEXT NOT NULL DEFAULT '',
    source_ts   TIMESTAMPTZ,
    msg_type    TEXT NOT NULL DEFAULT 'text',     -- text / image / audio / file
    file_paths  TEXT[],
    sender_id   TEXT,
    embedding   vector(768),                      -- nullable; async-filled
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Pipeline queue (user messages only; NULL for assistant/tool msgs)
    reply_status     TEXT,                        -- received/ready/processing/done
    reply_channel_id TEXT,                        -- IM-abstract card handle (set on ACK)
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

## Scheduled Tasks

Current implementation: a background goroutine reads static jobs from
`config.yaml`, runs `agent.Handle()` directly, and pushes the result via
Feishu.

Planned implementation: cron schedules live in PostgreSQL and only create
async tasks. The async task worker runs the Agent, records traces, and
emits outbox events. This makes scheduled work visible, editable,
recoverable, and consistent with user-created background tasks.

Current async task status: `tasks` persistence, lease-based worker claim,
`create_async_task`, `get_task_status`, and `cancel_task` are implemented.
The worker executes queued task prompts through the existing Agent and
stores the final result on the task row. Completed and failed async tasks
now emit `outbox_events`; the Feishu outbox presenter delivers those
events only when the chat presentation lock is free.

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

cron:
  jobs: []
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
    agent.go                 Two-phase orchestrator + streaming synthesizer;
                             HandleStreamQueued for pipeline mode,
                             HandleStream/Handle for CLI/Cron
    harness.go               Context curation: history + two prompt builders
    prompts.go               OrchestratorPrompt + SynthesizerPrompt constants
    event.go                 Event types: Thinking, ToolCall, ToolResult,
                             ReplyDelta, Reply, Error
  config/config.go           YAML config + env overrides
  cron/cron.go               Daily job scheduler
  docindex/ingest.go         Document RAG ingester (chunk + embed)
  embedding/
    client.go                Embedding HTTP client (OpenAI-compatible)
    worker.go                Async worker: embed unembedded messages/memories/chunks
  feishu/
    handler.go               Webhook inbound (INSERT + channel push +
                             spawn ProcessMedia goroutine); media
                             processing (download, Whisper, LLM correction)
    dispatcher.go            Per-chat MPSC channel, thinking card on pop,
                             ReadyCh wait, agent dispatch, crash recovery
    adapter.go               Agent events → card PATCHes (throttled streaming)
    client.go                Feishu API: reply, send, upload image, download
    card.go                  Interactive card builders + per-tool humanizer
    event.go                 Event type definitions
    dedup.go                 Event ID deduplication
    outbox_presenter.go      Outbox events → proactive Feishu cards
    presentation.go          Per-chat presentation lock
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
      005_async_tasks.sql    tasks + outbox_events
  task/
    worker.go                Lease-based async task worker
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
    async_task.go            Create/status/cancel durable async tasks
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

- **Single instance only.** Per-chat serialization relies on in-memory
  `sync.Map` + goroutine-per-chat. The DB uses `FOR UPDATE SKIP LOCKED`
  which is multi-process-safe at the claim level, but the thinking card
  lifecycle and ReadyCh coordination assume a single server process. To
  support horizontal scaling, the per-chat "only one consumer" guarantee
  would need to be promoted to a DB advisory lock or lease mechanism.

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
- **Store first, process later** — MQTT QoS=1: persist the message before
  any processing or acknowledgment. Crash at any point = no data loss
- **Per-chat serial, cross-chat parallel** — MPSC channel per chat with
  async media goroutines; `ReadyCh` synchronizes content readiness without
  polling or state tracking
- Builtin skills compiled in with platform build tags; user skills are files
- Skills grow organically through use, not through code changes
- All media saved to workspace for memory system reference
- Local models handle everything; API cost approaches zero
- **Use the best model for each role** — commercial APIs for orchestrator
  (tool calling quality) and synthesizer (answer quality); local models
  for fallback, transcription, and embedding. The harness guardrails
  (budgets, early-abort, retry) remain as safety nets but should rarely
  fire with capable models
