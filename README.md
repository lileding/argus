# Argus

A personal AI assistant. Runs as a single binary, connects to Feishu (Lark) over WebSocket, orchestrates LLMs to handle conversation, web search, structured data, audio, images, document RAG, and scheduled tasks.

> One assistant, one memory, one timeline. Not a chatbot.

See [DESIGN.md](DESIGN.md) for the architecture.
See [ROADMAP.md](ROADMAP.md) for what's next.

## Highlights

- **Two-phase agent** — Orchestrator (tool calling) → Synthesizer (answer composition). Each phase can use a different model from a different provider.
- **Multi-backend** — OpenAI Chat Completions, OpenAI Responses (GPT-5+ reasoning + tools), Anthropic (extended thinking), Gemini, DeepSeek. All via `reqwest`, no SDKs.
- **Async tasks** — long-running research jobs run in the background while the Agent keeps processing new messages.
- **Cron** — persistent scheduled tasks. User says "每天九点告诉我天气", model creates a cron with a self-contained execution prompt.
- **Document RAG** — uploaded files (PDF/DOCX/text) are chunked, embedded, and queryable via `search_docs`.
- **Vision** — images sent in Feishu are base64-encoded and passed to vision-capable models.
- **Audio** — Feishu voice messages are transcribed via OpenAI Whisper.
- **Memory** — sliding window + pgvector semantic recall + pinned memories + long-reply summaries.
- **Skills** — SKILL.md files (same format as Claude Code) define domain-specific behaviors. Loaded once at startup; activated on demand.
- **Tracing** — every request recorded with full tool calls, normalized arguments, timings, model names.
- **No `Arc`, no `tokio::spawn`** — five peer services run concurrently via `tokio::join!`, sharing references.

## Architecture (one screen)

Five peer services driven by `tokio::join!`:

| Service | Role |
|---|---|
| **Gateway** | IM adapters (Feishu); WS inbound, media, card rendering. Single outbound MPSC; an internal dispatcher routes each `Notification` to the matching IM by `sink` prefix |
| **Agent** | Top-level dispatcher routes each inbound message (by `sink`) and each task (by `channel_id`) to a per-channel `Processor`. Each Processor runs `sync_message_loop` (sequential) + `async_task_loop` (parallel) via `join!` |
| **Embedder** | Background: embeds rows, summarizes long replies, ingests docs |
| **Recovery** | Replays unreplied messages on restart and every 5 min |
| **Scheduler** | Scans `crons` every 60s, fires due jobs into the Agent |

Strict three-layer import direction: **Gateway → Agent → Upstream**.

## Quick Start

argus runs in docker. Build the image, then deploy alongside a
PostgreSQL+pgvector container.

```bash
# Prereqs: Docker, Rust toolchain (edition 2024) — only needed for `make check`

git clone https://github.com/lileding/argus.git
cd argus

# Build the runtime image (musl-static + alpine, ~107 MB)
make image
```

Create a working directory with your config and a compose file
(example below). The `workspace/` path inside this repo is gitignored
so you can develop your test env there without leaking secrets.

```bash
mkdir -p mywork/{media,user,skills,backups}
cp config.example.toml mywork/config.toml
# Edit: set Feishu app_id/app_secret, pick upstreams + API keys
#       set [database] dsn = "postgres://argus:argus@db:5432/argus?sslmode=disable"
```

`mywork/docker-compose.yml`:

```yaml
services:
  db:
    image: pgvector/pgvector:pg16
    environment:
      POSTGRES_DB: argus
      POSTGRES_USER: argus
      POSTGRES_PASSWORD: argus
    volumes:
      - argus_pgdata:/var/lib/postgresql/data
    ports: ["5432:5432"]
    networks: [argus-net]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U argus -d argus"]
      interval: 5s
      retries: 10

  argus:
    image: argus:latest
    depends_on:
      db: { condition: service_healthy }
    volumes:
      - ./config.toml:/app/config.toml:ro
      - ./media:/app/workspace/media
      - ./user:/app/workspace/user
      - ./skills:/app/workspace/skills:ro
    networks: [argus-net]
    restart: unless-stopped

volumes:
  argus_pgdata:

networks:
  argus-net:
    driver: bridge
```

```bash
cd mywork && docker compose up -d
```

## Configuration

TOML config (default `~/.config/argus/argus.toml`, override with `--config`).

Named **upstreams** + per-role model selection:

```toml
workspace_dir = "~/.local/share/argus"

[gateway.feishu]
app_id     = "cli_..."
app_secret = "..."

[gateway.feishu.transcription]
upstream   = "openai"
model_name = "whisper-1"

[upstream.openai-r]                # GPT-5+ with reasoning + tools
type    = "openai-response"
api_key = "sk-..."

[upstream.gemini]
type    = "gemini"
api_key = "AIza..."

[upstream.deepseek]                # OpenAI-compatible API
type     = "openai"
base_url = "https://api.deepseek.com/"
api_key  = "sk-..."

[agent.orchestrator]
upstream   = "openai-r"
model_name = "gpt-5.5"
max_tokens = 32768

[agent.synthesizer]
upstream   = "gemini"
model_name = "gemini-2.5-flash-lite"
max_tokens = 32768

[embedder]
upstream   = "openai"
model_name = "text-embedding-3-small"
dimensions = 768                   # truncate to match DB schema vector(768)

[embedder.summarizer]
upstream   = "gemini"
model_name = "gemini-2.5-flash-lite"

[database]
dsn = "postgres://argus:argus@localhost:5432/argus?sslmode=disable"
```

See [config.example.toml](config.example.toml).

### Upstream types

| Type | Endpoint | Use |
|---|---|---|
| `openai` | `/v1/chat/completions` | OpenAI Chat Completions, DeepSeek, Groq, local servers |
| `openai-response` | `/v1/responses` | GPT-5+ with reasoning + tools (Chat Completions rejects this combo) |
| `anthropic` | `/v1/messages` | Native Anthropic API; supports extended thinking |
| `gemini` | OpenAI-compatible Gemini endpoint | Reuses the OpenAI client |

### Recommended setups

| Orchestrator | Synthesizer | ~Cost/mo | Notes |
|---|---|---|---|
| GPT-5.5 (`openai-response`) | Gemini 2.5 Flash Lite | $15 | Best instruction following |
| Claude Sonnet 4.6 | Gemini 2.5 Flash Lite | $10 | Strong reasoning, native thinking |
| DeepSeek-V4-Pro | DeepSeek-V4-Flash | $2 | Best value |

## Tools

| Tool | Purpose |
|---|---|
| `finish_task` | Sentinel — orchestrator → synthesizer transition |
| `current_time` | Date/time with timezone |
| `search` | Web search (Tavily, DuckDuckGo fallback) |
| `fetch` | URL → readable text |
| `read_file` / `write_file` | File I/O (write restricted to `user/`) |
| `cli` | Shell commands on host |
| `db` | Structured data (CLI+JSON, 7 verbs, no raw SQL) |
| `remember` / `forget` | Pinned memories |
| `search_docs` / `list_docs` | Document RAG |
| `search_history` | Conversation history search |
| `activate_skill` | Load a skill's full instructions on demand |
| `create_task` | Spawn an async background task |
| `create_cron` / `list_crons` / `cancel_cron` / `update_cron` | Scheduled tasks |

## Development

```bash
make all       # cargo build (host)
make check     # fmt + clippy + test (host)
make test      # cargo test --workspace (host)
make image     # docker build -t argus:latest .
```

Iterate by `make image` then `docker compose up -d argus` from your
working directory (e.g. `workspace/`). Compose does not auto-rebuild —
each code change requires an explicit `make image`.

## License

[MIT](LICENSE)
