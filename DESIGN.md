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
Message source (Feishu webhook / CLI)
        ↓
   Harness (context curation)
        ↓
   LLM (local, OpenAI-compatible API)
        ↓
   Tool call ──→ Sandbox.Exec ──→ Result ──→ Continue loop
        ↓                ↓
   Send reply    ┌───────┴───────┐
        ↓        Local        Docker
   Store to DB  (sh -c)   (docker run)
```

### Two-Phase Execution

Tool calls and sandbox execution are **orthogonal**:
- **Phase 1 (Tool Layer)**: LLM outputs tool calls — defines WHAT to do
- **Phase 2 (Sandbox Layer)**: Sandbox executes — defines WHERE to run

Sandboxes are configurable:
- `local` — direct host execution via `bash -c` (development)
- `docker` — isolated container execution (production, default image: `argus-sandbox`)

---

## Harness Engineering (Context Curation)

**Core principle: context is a scarce, finite resource. Only high-signal content goes in.**

The LLM never sees raw conversation history. Every request goes through a curation pipeline:

```
User message arrives
    ↓
[1] System Prompt Assembly
    - Base prompt (from config)
    - Environment: current time, home dir, workspace path
    - Builtin skill prompts (compiled in, always present)
    - User skill catalog (name + description only)
    - Skill accumulation guide
    ↓
[2] History Curation
    - Load recent N messages from store
    - Keep only: user messages + assistant final replies
    - Remove: tool_call / tool_result noise
    ↓
[3] Assemble: [system prompt] + [curated history] + [user message]
    ↓
[4] Tool output safety
    - Results truncated to 16KB max (prevents context blowup)
    ↓
    Send to model
```

---

## Model Strategy

All messages are handled by a local LLM via OpenAI-compatible API. No routing layer — the model decides implicitly through tool calling what capabilities to use.

The model can be any OpenAI-compatible endpoint: Gemma, Llama, Qwen, etc., served by omlx, vLLM, Ollama, LM Studio, or any other backend. Prefix KV cache is handled by the inference engine transparently.

---

## Agent Loop

```
1. Receive message
2. Harness curates context (system prompt + history + skills)
3. Call model
4. Parse response:
   - tool_calls → execute in sandbox → inject result (truncated) → back to 3
   - stop → send reply
5. Store user message + reply to DB
6. Log token usage per iteration
```

Max iterations is configurable (default: 10) to prevent infinite loops.

---

## Skills

Skills follow the [Agent Skills open standard](https://agentskills.io) (same SKILL.md format as Claude Code).

### Two Types

**Builtin skills** — compiled into the binary, platform-conditional (`//go:build unix` / `windows`). Their full prompt is always injected into the system prompt. Example: `posix-cli` teaches the model how to use grep, find, awk, sed, jq, pipelines.

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
| `save_skill` | Create/update SKILL.md files | Always |
| `activate_skill` | Load a skill's full instructions on demand | Always |
| `search` | Web search | Always (stub) |
| `db` | Read-only SQL queries | When DB available |
| `db_exec` | Write SQL (INSERT/UPDATE/CREATE TABLE) | When DB available |

### Safety

- `read_file` / `write_file`: paths restricted to workspace, `..` traversal blocked, `~` rejected with helpful error
- `cli`: execution delegated to sandbox (local or Docker with network/memory/CPU limits)
- `db_exec`: DROP on protected tables (messages, schema_migrations) is blocked
- Tool output truncated to 16KB to prevent context window overflow

---

## Memory

**Current**: fixed window (last 20 messages) + history curation (tool noise removed).

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

Business tables (e.g. food_log) are created dynamically by the agent via `db_exec`, not part of the core schema.

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
  base_url: "http://localhost:11434/v1"
  model_name: "gemma3:27b"
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
  type: local          # "local" or "docker"
  image: argus-sandbox  # docker image
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
    harness.go              Context curation: prompt assembly, history filtering
  config/config.go          YAML config + env overrides
  cron/cron.go              Daily job scheduler
  feishu/                   Webhook handler + API client + event dedup
  model/
    model.go                Client interface
    openai.go               OpenAI-compatible implementation
    types.go                Message, ToolCall, Response, Usage
  sandbox/
    sandbox.go              Sandbox interface: Exec(ctx, command, workDir)
    local.go                Host execution (bash -c)
    docker.go               Container execution (docker run)
  skill/
    index.go                SkillIndex: thread-safe in-memory index
    loader.go               FileLoader: scan .skills/, background rescan
    builtin.go              BuiltinSkills() dispatcher
    builtin_unix.go         POSIX CLI skill (grep/find/awk/sed/jq)
    builtin_windows.go      PowerShell CLI skill
  store/
    store.go                Store interface
    postgres.go             PostgreSQL + embedded migrations
    memory.go               In-memory store (CLI testing)
  tool/
    tool.go                 Tool interface + ToModelToolDef
    registry.go             Tool registry with filtered lookup
    read_file.go / write_file.go  (in file.go)
    cli.go                  Shell command tool (delegates to sandbox)
    db.go / db_exec.go      Database query/exec tools
    search.go               Web search (stub)
    save_skill.go           Create SKILL.md files
    activate_skill.go       Load skill prompt on demand
    context.go              ChatID context key for tools
```

---

## Tech Stack

| Component | Choice |
|-----------|--------|
| Language | Go |
| IM | Feishu Bot API |
| LLM | Any local model via OpenAI-compatible API (omlx, vLLM, Ollama) |
| Database | PostgreSQL |
| Sandbox | Local (dev) / Docker (prod) |
| Skills | SKILL.md files (Agent Skills open standard) + compiled builtins |
| Binary | Single daemon, `--workspace` flag |

---

## Principles

- No "sessions", only a timeline
- Context is scarce — only high-signal tokens (Harness Engineering)
- Tool calls and execution are orthogonal (tool layer × sandbox layer)
- Builtin skills compiled in with platform build tags; user skills are files
- Skills grow organically through use, not through code changes
- Local model handles everything; API cost approaches zero
