# Argus — Claude Code Instructions

## Pre-commit Review Workflow

Every `git commit` must go through a codex review cycle:

1. **Before committing**, spawn a `codex` CLI Agent to review the staged diff. The review prompt must include:
   - Read all changed files
   - Check against DESIGN.md architectural requirements
   - Check against the Design Principles below (mandatory)
   - Look for: correctness, race conditions, error handling, Rust idiom, test coverage
   - Format as: 主要问题 (numbered, with file:line) / 次要问题 / 已做好的点

2. **Show the review to the user** — paste the full review output.

3. **Fix issues** the user agrees should be fixed. Iterate:
   - Fix → re-run `cargo fmt && cargo clippy --workspace -- -D warnings && cargo test --workspace`
   - If needed, re-review with codex
   - Repeat until review passes or remaining issues are explicitly deferred

4. **Then ask to confirm commit**.

## Design Principles (STRICT — codex must enforce)

### Read DESIGN.md

### High Cohesion, Low Coupling
- Each module owns its internal logic. Callers pass config, not pre-built internals.
- `Agent::new(&config.agent, &upstream)` — not `Agent::new(orchestrator_client, synthesizer_client)`.
- `Gateway::new(&config.gateway, &agent, workspace_dir)` — not a list of individually constructed IMs.
- If a caller needs to know implementation details to construct something, the API is wrong.

### Strict Encapsulation
- Internal types stay internal. Use `pub(crate)` or `pub(super)`, not `pub`.
- Sub-module types (e.g. `OpenAiClient`, `AnthropicClient`) must never appear in parent module's public API.
- Trait objects (`Arc<dyn Client>`, `Arc<dyn Im>`) at module boundaries, concrete types inside.

### Minimal Public Surface
- Every `pub fn`, `pub struct`, `pub trait` must justify its existence.
- If only one caller uses it, consider making it `pub(crate)` or moving it closer.
- Config structs are public (deserialization). Internal implementation structs are not.

### Symmetric Initialization
- `main.rs` creates top-level objects from their config sections:
  ```rust
  let upstream = Upstream::new(&config.upstream);
  let agent = Agent::new(&config.agent, &upstream)?;
  let gateway = Gateway::new(&config.gateway, &agent, &config.workspace_dir);
  ```
- No module's internals leak into `main.rs`.

## Build Commands

```
make check    # cargo fmt --check + cargo clippy + cargo test
make run      # RUST_LOG + cargo run
```

## Architecture

Three decoupled layers: **Gateway → Agent → Upstream**

- Gateway (`src/gateway/`): IM adapters (feishu, etc.), inbound + outbound rendering
- Agent (`src/agent.rs`): task scheduler, two-phase execution (orchestrator → synthesizer)
- Upstream (`src/upstream/`): model provider clients (OpenAI, Anthropic, Gemini)

Hierarchy: Gateway → Im (feishu/slack) → Channel (per-chat, future)

Import direction: Gateway imports Agent types. Agent imports Upstream. Agent never imports Gateway.

## Shutdown Order

1. Agent stops first (finishes in-flight task, closes events channels)
2. Gateway stops second (drains outbound queue, then exits)

## Language

- Code, comments, commit messages: English
- Conversational replies to user: Chinese

## Test envirionment

- Workspace directory is 'workspace'
- Config file is 'workspace/config.toml'
- PostgreSQL runs in docker called 'mm-db'

