# Argus

A personal AI assistant that runs as a server, connects to Feishu (Lark), and orchestrates multiple AI models to handle conversations, search the web, manage structured data, transcribe audio, and understand images.

## Architecture

**Two-phase agent**: an Orchestrator (tool calling) collects information, then a Synthesizer (answer generation) composes the response. Each phase can use a different model from a different provider.

**Multi-backend**: supports OpenAI, Anthropic, and Google Gemini APIs. Each agent role (orchestrator, synthesizer, transcription) independently selects its provider and model via config.

**Structured data**: a single `db` tool with CLI-style commands (`query`, `insert`, `count`, etc.) replaces raw SQL. The model never writes SQL — the tool generates parameterized queries internally.

**Full tracing**: every request is recorded with tool calls, arguments, results, timing, and model names. Designed as the foundation for future skill induction.

See [DESIGN.md](DESIGN.md) for the complete architecture documentation.

## Quick Start

```bash
# Prerequisites: Go 1.26+, PostgreSQL with pgvector, Rust toolchain (for LaTeX rendering)

# Clone and build
git clone https://github.com/lileding/argus.git
cd argus
make build

# Set up PostgreSQL
make up  # starts PostgreSQL via docker-compose

# Configure
cp config.example.yaml ~/.argus/config.yaml
# Edit ~/.argus/config.yaml — set your API keys and Feishu credentials

# Run
./bin/argus
```

## Configuration

Argus uses named **upstreams** (model providers) and per-role model selection:

```yaml
upstreams:
  openai:
    type: openai
    base_url: "https://api.openai.com/v1"
    api_key: "sk-..."
  anthropic:
    type: anthropic
    api_key: "sk-ant-..."
  gemini:
    type: gemini
    api_key: "AIza..."
  local:
    type: openai
    base_url: "http://localhost:8000/v1"
    api_key: "omlx"

model:
  orchestrator:
    upstream: openai
    model_name: "gpt-5.4"
    max_tokens: 4096
  synthesizer:
    upstream: gemini
    model_name: "gemini-2.5-flash-lite"
    max_tokens: 32768
  transcription:
    upstream: local
    model_name: "whisper-large-v3"
```

See [config.example.yaml](config.example.yaml) for all options.

## Features

- **Feishu integration**: text, image, audio, rich text, and file messages
- **Vision**: images sent via Feishu are passed to vision-capable models (GPT-5.4, Gemini, Claude)
- **Audio**: Whisper transcription with confidence-based LLM correction
- **Web search**: Tavily (LLM-optimized) with DuckDuckGo fallback
- **Structured data**: CLI+JSON `db` tool (query, insert, update, count, create, describe, list)
- **Skills**: SKILL.md files define domain-specific behaviors (e.g. food tracking)
- **Memory**: sliding window + semantic recall (pgvector) + pinned memories
- **LaTeX**: display math rendered to PNG via embedded RaTeX (Rust/CGo)
- **Streaming**: SSE token streaming with Feishu card updates
- **Tracing**: full request traces with tool calls, timing, and normalized args

## Tools

| Tool | Purpose |
|------|---------|
| `search` | Web search (Tavily / DuckDuckGo) |
| `fetch` | Fetch URL content as readable text |
| `db` | Structured data (query/insert/update/count/create/describe/list) |
| `read_file` | Read workspace files |
| `write_file` | Write to `.users/` directory |
| `cli` | Execute shell commands via sandbox |
| `activate_skill` | Load skill instructions on demand |
| `remember` / `forget` | Manage pinned memories |
| `current_time` | Current date/time |
| `finish_task` | Signal orchestrator → synthesizer transition |

## Development

```bash
make build      # build binary (includes RaTeX Rust library)
make test       # run tests
make run        # build + run with ./workspace
```

## License

[MIT](LICENSE)
