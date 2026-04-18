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

Both phases use the same model; the difference is purely the system prompt
and the tool list (empty for Phase 2).

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
| **text** | Handler stores raw JSON → `ProcessMedia` extracts text (instant) |
| **image** | Handler stores raw JSON → `ProcessMedia` downloads to `.files/` |
| **post** (rich text) | Handler stores raw JSON → `ProcessMedia` extracts text + downloads images |
| **audio** | Handler stores raw JSON → `ProcessMedia` downloads `.opus` → Whisper → LLM correction |
| **file** (PDF, docx, etc.) | Handler stores raw JSON → `ProcessMedia` downloads to `.files/` → registers for RAG |

All media processing runs in an async goroutine (`ProcessMedia`) spawned
by the Handler. The Handler's inbound path is sub-millisecond: INSERT +
channel push + goroutine spawn. The Dispatcher opens the thinking card
immediately on pop, then blocks on the goroutine's `ReadyCh` until
content is ready.

### Multi-Turn Vision

Images are saved to `workspace/.files/`. When building history context, messages referencing saved images are re-loaded from disk as multimodal content, so the model can see and discuss previously sent images across conversation turns.

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

Every transcription is post-processed by the main LLM to add punctuation and fix misheard words. The raw and corrected text are both logged for debugging.

### Document RAG

Non-image files (PDF, docx, etc.) go through the `docindex` ingester:
download → extract text via sandbox CLI (`pdftotext`, `pandoc`) → chunk →
embed each chunk → store in `chunks` table with pgvector index. The model
can semantically search ingested documents via the agent's semantic recall.

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
| **Per-tool budget** | `search`≤3, `fetch`≤4, `db`≤6, `cli`≤5, `write_file`≤3, `db_exec`≤5, `remember`≤3 per turn | Harness rejects further calls without dispatching; returns error "budget exhausted, call finish_task NOW" |
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

## Model Strategy

All messages are handled by a local LLM via an OpenAI-compatible endpoint (omlx, vLLM, or similar). No routing layer — the orchestrator decides implicitly through tool calling what capabilities to use.

### Model Choice

| Role | Model | Notes |
|------|-------|-------|
| Chat | **Qwen3.5-35B-A3B 8bit (MoE)** — production | 3B active on 35B total. Hermes-style tool calling is strict — very few loop/skip bugs observed in production. |
| Transcription | Whisper Large v3 | `/v1/audio/transcriptions` with domain-prompt vocabulary (composers, tech, finance) |
| Embedding | modernbert-embed-base (768 dim) | Async worker batches unembedded messages/memories/chunks every 2 s |

KV cache 4-bit quantization is recommended; unified-memory Macs are
bandwidth-bound for decode so compressing KV cache directly buys speed.

**Dense models are not recommended.** Tested dense chat models on the
same hardware had two disqualifying failure modes: (1) bandwidth-bound
decode at long context (~5 tok/s), and (2) unreliable instruction
following under the two-phase contract (skipping tool calls, looping
the same search 20+ times with trivial query variations, ignoring
system-level loop-break nudges). The harness budgets in the
Orchestrator were originally added to survive exactly this behavior;
on MoE models with stricter tool-calling, they rarely fire.

### Hardware baseline (M4 Max 128 GB)

| Config | Prefill | Decode (long ctx) | Notes |
|---|---:|---:|---|
| Qwen3.5-35B-A3B 8bit + KV 4bit | ~180 tok/s | ~30 tok/s | **current production** |
| Qwen3.5-35B-A3B 4bit + KV 4bit | ~200 tok/s | 40+ tok/s | faster, slightly less accurate |
| 31B dense 8bit (reference) | ~190 tok/s | 4–9 tok/s | bandwidth-bound ceiling |

MoE is the correct architecture for this deployment envelope: the same
model file is ~5× faster at decode than a comparable dense model on this
hardware.

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

**User skills** — SKILL.md files in `workspace/.skills/`, created by the agent via `save_skill` or by the user manually. Loaded at startup, background-rescanned every 30 s. Only name + description appear in the orchestrator's system prompt catalog; full prompt loaded via `activate_skill` tool on demand.

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
name: calorie
description: "Track daily food intake and calories."
tools:
  - db
  - db_exec
---

## Calorie Tracking

When the user mentions eating or drinking...
```

### Skill Accumulation

When the agent successfully completes a new type of recurring task, it uses `save_skill` to persist its approach as a reusable SKILL.md file. Over time, the agent's capabilities grow organically through use.

---

## Tools

| Tool | Purpose | Availability |
|------|---------|-------------|
| `read_file` | Read file contents (workspace-relative paths) | Always |
| `write_file` | Write file contents (auto-creates directories) | Always |
| `cli` | Execute shell commands via sandbox | Always |
| `search` | Web search (DuckDuckGo, no API key) — **budget 3/turn** | Always |
| `fetch` | URL → readable text — **budget 4/turn** | Always |
| `current_time` | Date/time with timezone support | Always |
| `save_skill` | Create/update SKILL.md files | Always |
| `activate_skill` | Load a skill's full instructions on demand | Always |
| `finish_task` | Sentinel — signals orchestrator → synthesizer transition | Always |
| `db` | Read-only SQL queries — **budget 6/turn**, sandboxed (see below) | When DB available |
| `db_exec` | Write SQL (INSERT/UPDATE/CREATE TABLE), sandboxed (see below) | When DB available |
| `remember` | Pin a persistent memory (pgvector indexed) | When DB available |
| `forget` | Deactivate a pinned memory by ID | When DB available |

### Safety

- `read_file` / `write_file`: paths restricted to workspace, `..` traversal blocked, `~` rejected with helpful error suggesting `cli` tool
- `cli`: execution delegated to sandbox (local or Docker with network/memory/CPU limits)
- `db` / `db_exec`: all model SQL passes through `internal/sqlsandbox` (see below)
- Tool output truncated to 16 KB to prevent context overflow
- All tool calls logged with full arguments before execution

### DB Sandboxing

The model never names a system table. Every SQL statement passed to `db` or
`db_exec` is parsed by PostgreSQL's own parser (via libpg_query) and every
user-level table/index identifier gets the hidden prefix `argus_`. The model
writes `SELECT * FROM food_log`; the tool executes
`SELECT * FROM argus_food_log`. Error messages are scrubbed on the way back
so the model never sees the prefix.

Three machine-enforced rules give the **non-collision guarantee**:

1. System tables (`messages`, `schema_migrations`, `memories`, `documents`,
   `chunks`) never start with `argus_` — the namespace is reserved. A guard
   test walks `internal/store/migrations/*.sql` and fails CI if any future
   migration reserves a model-namespace name.
2. Every model-issued `RangeVar` is prefixed. Full AST coverage by
   `sandbox_test.go` catches a stray unhandled case.
3. Inputs that already contain an `argus_`-prefixed identifier are rejected
   outright — closes the double-prefix escape.

Additional rejections: schema-qualified references
(`public.x`, `information_schema.*`, `pg_catalog.*`), multi-statement SQL
(`SELECT 1; DROP TABLE messages`), and `DROP` of non-relation object types
(schemas, functions, roles, …).

The old `protectedTables` DROP-string-match is gone — the namespace split
makes it redundant.

**Upgrading from a pre-sandbox DB**: if a pre-sandbox deployment wrote
tables under unprefixed names, a one-shot manual rename brings them into
the new namespace:

```sql
-- For each model-created table that used to exist unprefixed:
ALTER TABLE food_log RENAME TO argus_food_log;
-- Indexes and sequences rename automatically with the table in PostgreSQL.
```

Not automated because a shared PG instance may also host unrelated tables
(e.g. Mattermost in the same cluster).

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

IVFFlat cosine indexes on all three embedding columns. Agent-created business tables (e.g. `food_log`) live alongside these and are created dynamically via `db_exec`.

Database is optional — server mode falls back to memory store if PostgreSQL is unavailable, though semantic recall, pinned memories, and document RAG are disabled in that mode.

`docker-compose.yaml` provided for one-command PostgreSQL+pgvector setup: `make up`.

---

## Scheduled Tasks

Background goroutine runs a cron scheduler, independent of user interaction:
- Jobs defined in config (name, hour, minute, target chat_id, prompt)
- Each job runs `agent.Handle()` (synchronous wrapper over the two-phase stream) and pushes the result via Feishu

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

model:
  base_url: "http://localhost:8000/v1"
  model_name: "Qwen3.5-35B-A3B-8bit"     # MoE; 4bit also works (trade accuracy for speed)
  transcription_model: "whisper-large-v3"
  api_key: "omlx"
  max_tokens: 4096
  timeout: 240s

database:
  dsn: "postgres://argus:argus@localhost:5432/argus?sslmode=disable"

agent:
  max_iterations: 10
  context_window: 20
  skill_rescan: 30s

embedding:
  enabled: true
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

---

## Project Structure

```
cmd/argus/main.go            Entry point, --workspace and --mode flags
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
  model/
    model.go                 Client interface: Chat + ChatStream +
                             ChatWithEarlyAbort + Transcribe
    openai.go                OpenAI-compatible impl (JSON + SSE + multipart);
                             ChatWithEarlyAbort parses tool_call deltas
                             from SSE for early text-only abort
    types.go                 Message (multimodal), ToolCall, Response, Usage
  render/
    renderer.go              Processor: markdown → LaTeX images → markers
    latex.go                 LaTeX detection + RaTeX PNG rendering
  sandbox/
    sandbox.go               Sandbox interface: Exec(ctx, command, workDir)
    local.go                 Host execution (bash -c)
    docker.go                Container execution (docker run)
  sqlsandbox/
    sandbox.go               SQL rewriter: AST-walk, prefix RangeVars,
                             reject schema-qualified / pre-prefixed /
                             multi-statement inputs; StripPrefix for errors
    sandbox_test.go          Accept/reject corpus + migrations invariant guard
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
    file.go                  read_file + write_file (workspace-restricted)
    cli.go                   Shell commands (delegates to sandbox)
    search.go                DuckDuckGo web search
    fetch.go                 URL fetcher with HTML-to-text conversion
    current_time.go          Date/time with timezone support
    db.go / db_exec.go       Database read/write
    save_skill.go            Create SKILL.md files
    activate_skill.go        Load skill prompt on demand
    remember.go / forget.go  Pinned memory management
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
| Chat Model | Local MoE — Qwen3.5-35B-A3B 8bit in production, via omlx / vLLM |
| Transcription | Whisper Large v3 via `/v1/audio/transcriptions` |
| Embedding | modernbert-embed-base (768 dim) via `/v1/embeddings` |
| Database | PostgreSQL + pgvector (optional, memory store fallback) |
| Sandbox | Local (dev) / Docker (prod) |
| Web Search | DuckDuckGo HTML (no API key) |
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

- **Document RAG retrieval.** The `docindex` ingester chunks and embeds
  uploaded files into the `chunks` table, but the agent has no explicit
  retrieval path — `loadHistory` only searches message embeddings, not
  chunk embeddings. Two options: (a) add chunk search to `loadHistory`
  for automatic context injection, (b) provide a `search_documents` tool
  for the Orchestrator. Option (b) is more controllable and avoids
  injecting irrelevant chunks into every query.

- **Multi-modal image re-injection.** Current implementation scans
  `.files/` and matches filenames via `strings.Contains(content, name)`.
  This is fragile (false positives on substring matches, O(n) directory
  scan). A cleaner approach: `ProcessMedia` populates
  `StoredMessage.FilePaths`, and `curateHistory` re-injects images
  strictly from that field. This also enables proper cleanup of expired
  files and per-message attachment tracking.

---

## Design Trade-offs (Deliberate Non-Goals)

Considered and rejected. Documenting the reasoning so future contributors
don't re-propose them without new evidence.

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
- Prefer MoE on unified-memory Macs — bandwidth-bound decode, dense 30B hits ~5 tok/s ceiling while a 35B/3B MoE at 8bit holds ~30 tok/s in production
