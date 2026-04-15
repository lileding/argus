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

```
Feishu message (text / image / audio / file / post)
        ↓
   Media processing (download, save to .files/, transcribe audio)
        ↓
   Harness (context curation)
        ↓
   LLM (local, OpenAI-compatible API)
        ↓
   Tool call ──→ Sandbox.Exec ──→ Result (truncated) ──→ Continue loop
        ↓                ↓
   Render reply  ┌───────┴───────┐
   (md → feishu) Local        Docker
        ↓       (bash -c)   (docker run)
   Send via Feishu API
        ↓
   Store to DB
```

### Two-Phase Execution

Tool calls and sandbox execution are **orthogonal**:
- **Phase 1 (Tool Layer)**: LLM outputs tool calls — defines WHAT to do
- **Phase 2 (Sandbox Layer)**: Sandbox executes — defines WHERE to run

Sandboxes are configurable:
- `local` — direct host execution via `bash -c` (development)
- `docker` — isolated container execution (production, image: `argus-sandbox`)

---

## Multimodal Input

Argus handles all Feishu message types natively:

| Message Type | Processing |
|-------------|-----------|
| **text** | Direct to model |
| **image** | Download → save to `.files/` → base64 data URL via OpenAI vision API |
| **post** (rich text) | Extract text + images, build multimodal message |
| **audio** | Download → save to `.files/` → Whisper transcription → LLM post-processing → text |
| **file** (PDF, etc.) | Download → save to `.files/` → model uses `cli` tool to process (e.g. `pdftotext`) |

### Multi-Turn Vision

Images are saved to `workspace/.files/`. When building history context, messages referencing saved images are re-loaded from disk as multimodal content, so the model can see and discuss previously sent images across conversation turns.

### Audio Pipeline

```
Feishu audio → download → save .opus to .files/
    → Whisper v3 transcription (with domain vocabulary prompt)
    → LLM post-processing (add punctuation, fix domain terms)
    → corrected text sent to model
```

The transcription prompt includes hints for:
- Mixed Chinese/English code-switching
- Technology terms (API, Kubernetes, Docker, LLM, Maxwell, Transformer)
- Finance terms (ETF, ROI, derivatives, hedge fund)
- Arts terms (sonata, concerto, Chopin, Scriabin, Grieg, Dvořák)

Every transcription is post-processed by the main LLM to add punctuation and fix misheard words. The raw and corrected text are both logged for debugging.

---

## Harness Engineering (Context Curation)

**Core principle: context is a scarce, finite resource. Only high-signal content goes in.**

The LLM never sees raw conversation history. Every request goes through a curation pipeline:

```
User message arrives
    ↓
[1] System Prompt Assembly
    - Base prompt (with behavioral rules: act don't describe, search first)
    - Environment: today's date, home dir, workspace path
    - Builtin skill prompts (compiled in, always present)
    - User skill catalog (name + description only)
    - Skill accumulation guide
    ↓
[2] History Curation
    - Load recent N messages from store
    - Keep only: user messages + assistant final replies
    - Remove: tool_call / tool_result noise
    - Re-inject images from .files/ for multi-turn vision
    ↓
[3] Assemble: [system prompt] + [curated history] + [current user message]
    ↓
[4] Safety
    - Tool results truncated to 16KB max
    - Context assembled BEFORE saving user message (prevents duplicates)
    ↓
    Send to model
```

### System Prompt Policies

The default system prompt enforces:
- **Act, don't describe**: "NEVER say 'I will try to...' without making the actual tool call"
- **Search first**: "For ANY factual question, ALWAYS use the search tool first. Do NOT answer from memory alone."
- **Date awareness**: Today's date is always injected so the model never has a wrong year

---

## Model Strategy

All messages are handled by a local LLM via OpenAI-compatible API. No routing layer — the model decides implicitly through tool calling what capabilities to use.

### Model Configuration

| Role | Model | Purpose |
|------|-------|---------|
| Chat | Gemma 4 / Qwen / Llama (configurable) | Main conversation + tool calling |
| Transcription | Whisper Large v3 | Audio → text via `/v1/audio/transcriptions` |

Both models served by the same inference engine (omlx, vLLM, etc.) on the same endpoint. Prefix KV cache handled transparently by the engine.

---

## Agent Loop

```
1. Receive message (may be multimodal)
2. Harness curates context (system prompt + history + skills)
3. Save user message to store
4. Call model
5. Parse response:
   - tool_calls → execute in sandbox → inject result (truncated) → back to 4
   - no tool_calls → render reply → send via Feishu
6. Save assistant reply to store
7. Log token usage per iteration
```

Max iterations configurable (default: 10). The loop continues as long as the model returns tool calls, regardless of `finish_reason`.

---

## Output Rendering

Model output goes through a render layer before sending to Feishu:

- **Plain text** → `msg_type: "text"` (pass-through)
- **Markdown** (detected by `**`, `#`, `[](`, `` ``` ``) → `msg_type: "post"` (Feishu rich text)
  - `# heading` → post title
  - `**bold**` → bold element
  - `[text](url)` → link element
  - Code blocks → preserved as text
  - Lists → bullet prefixed
- **LaTeX** (future) → render to PNG via sandbox, upload as Feishu image

---

## Skills

Skills follow the [Agent Skills open standard](https://agentskills.io) (same SKILL.md format as Claude Code).

### Two Types

**Builtin skills** — compiled into the binary, platform-conditional (`//go:build unix` / `windows`). Their full prompt is always injected into the system prompt. Example: `posix-cli` teaches the model how to use grep, find, awk, sed, jq, pipelines, and enforces rules like "NEVER use `ls -R`, ALWAYS use `find`".

**User skills** — SKILL.md files in `workspace/.skills/`, created by the agent via `save_skill` or by the user manually. Loaded at startup, background-rescanned every 30s. Only name + description appear in the system prompt catalog; full prompt loaded via `activate_skill` tool on demand.

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
| `search` | Web search (DuckDuckGo, no API key needed) | Always |
| `fetch` | Fetch URL content as text (HTML → readable text) | Always |
| `current_time` | Get current date/time with timezone support | Always |
| `save_skill` | Create/update SKILL.md files | Always |
| `activate_skill` | Load a skill's full instructions on demand | Always |
| `db` | Read-only SQL queries | When DB available |
| `db_exec` | Write SQL (INSERT/UPDATE/CREATE TABLE) | When DB available |

### Safety

- `read_file` / `write_file`: paths restricted to workspace, `..` traversal blocked, `~` rejected with helpful error suggesting `cli` tool
- `cli`: execution delegated to sandbox (local or Docker with network/memory/CPU limits)
- `db_exec`: DROP on protected tables (messages, schema_migrations) is blocked
- Tool output truncated to 16KB to prevent context window overflow
- All tool calls logged with full arguments before execution

---

## Media Storage

All downloaded media is saved to `workspace/.files/`:

```
workspace/.files/
  img_v3_xxx.png          # Feishu images
  file_v3_xxx.opus        # Voice messages
  file_v3_xxx.pdf         # Uploaded files
```

Files are named by their Feishu file key + original extension. This directory serves as:
- Source for multi-turn image re-injection into context
- Archive for the future memory system to reference
- Input for model processing (e.g. `pdftotext` on PDFs)

---

## Memory

**Current**: fixed window (last 20 messages) + history curation (tool noise removed, images re-injected).

**Future**:
- Short-term summaries: generated async after conversations
- Long-term memory: daily user profile generation
- Semantic recall: pgvector cosine similarity search

---

## Data Model (PostgreSQL)

```sql
CREATE TABLE messages (
    id           BIGSERIAL PRIMARY KEY,
    chat_id      TEXT NOT NULL,
    role         TEXT NOT NULL,
    content      TEXT NOT NULL,
    tool_name    TEXT,
    tool_call_id TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX ON messages (chat_id, created_at);
```

Database is optional — server mode falls back to memory store if PostgreSQL is unavailable. Business tables (e.g. food_log) are created dynamically by the agent via `db_exec`.

`docker-compose.yaml` provided for one-command PostgreSQL setup: `make up`.

---

## Scheduled Tasks

Background goroutine runs a cron scheduler, independent of user interaction:
- Jobs defined in config (name, hour, minute, target chat_id, prompt)
- Each job runs `agent.Handle()` and pushes the result via Feishu

---

## Configuration

Single config file at `<workspace>/config.yaml`. CLI flag: `--workspace <dir>`.

```yaml
server:
  port: "8080"

feishu:
  app_id: ""
  app_secret: ""

model:
  base_url: "http://localhost:8000/v1"
  model_name: "gemma-4-27b"
  transcription_model: "whisper-large-v3"
  max_tokens: 4096
  timeout: 120s

database:
  dsn: "postgres://argus:argus@localhost:5432/argus?sslmode=disable"

agent:
  max_iterations: 10
  context_window: 20
  skills_dir: ".skills"
  skill_rescan: 30s

sandbox:
  type: local
  image: argus-sandbox
  timeout: 30s

cron:
  jobs: []
```

---

## Project Structure

```
cmd/argus/main.go           Entry point, --workspace and --mode flags
internal/
  agent/
    agent.go                Agent loop: tool call → sandbox exec → iterate
    harness.go              Context curation: prompt assembly, history filtering,
                            image re-injection, date injection
  config/config.go          YAML config + env overrides
  cron/cron.go              Daily job scheduler
  feishu/
    handler.go              Webhook handler: text/image/audio/file/post messages,
                            media download, audio transcription + LLM correction
    client.go               Feishu API: reply, send, upload image, download resources
    event.go                Event type definitions
    dedup.go                Event ID deduplication
  model/
    model.go                Client interface (Chat + Transcribe)
    openai.go               OpenAI-compatible implementation (chat + whisper)
    types.go                Message (multimodal), ContentPart, ToolCall, Response, Usage
  render/
    feishu.go               Markdown → Feishu post format converter
    latex.go                LaTeX detection + rendering (via sandbox)
  sandbox/
    sandbox.go              Sandbox interface: Exec(ctx, command, workDir)
    local.go                Host execution (bash -c)
    docker.go               Container execution (docker run)
  skill/
    index.go                SkillIndex: thread-safe in-memory index + catalog
    loader.go               FileLoader: load builtins + scan .skills/ + background rescan
    builtin.go              BuiltinSkills() dispatcher
    builtin_unix.go         POSIX CLI skill (grep/find/awk/sed/jq)
    builtin_windows.go      PowerShell CLI skill
  store/
    store.go                Store interface
    postgres.go             PostgreSQL + embedded migrations
    memory.go               In-memory store (CLI / no-DB mode)
  tool/
    tool.go                 Tool interface + ToModelToolDef
    registry.go             Tool registry with name-filtered lookup
    file.go                 read_file + write_file (workspace-restricted)
    cli.go                  Shell commands (delegates to sandbox)
    search.go               DuckDuckGo web search
    fetch.go                URL fetcher with HTML-to-text conversion
    current_time.go         Date/time with timezone support
    db.go / db_exec.go      Database read/write
    save_skill.go           Create SKILL.md files
    activate_skill.go       Load skill prompt on demand
    context.go              ChatID context key for tools
```

---

## Tech Stack

| Component | Choice |
|-----------|--------|
| Language | Go |
| IM | Feishu Bot API (text, image, audio, file, rich text) |
| Chat Model | Any local model via OpenAI-compatible API (omlx, vLLM, Ollama) |
| Transcription | Whisper Large v3 via `/v1/audio/transcriptions` |
| Database | PostgreSQL (optional, falls back to memory) |
| Sandbox | Local (dev) / Docker (prod) |
| Web Search | DuckDuckGo HTML (no API key) |
| Output Render | Markdown → Feishu rich text post format |
| Skills | SKILL.md files (Agent Skills standard) + compiled builtins |
| Deployment | Single binary, `--workspace` flag, `docker-compose.yaml` for PostgreSQL |

---

## Principles

- No "sessions", only a timeline
- Context is scarce — only high-signal tokens (Harness Engineering)
- Act, don't describe — model must call tools, not narrate plans
- Search first — factual questions use web search, not training data
- Tool calls and execution are orthogonal (tool layer × sandbox layer)
- Builtin skills compiled in with platform build tags; user skills are files
- Skills grow organically through use, not through code changes
- All media saved to workspace for future memory system reference
- Local models handle everything; API cost approaches zero
