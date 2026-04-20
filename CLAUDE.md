# Argus — Claude Code Instructions

## Pre-commit Review Workflow

Every `git commit` must go through a codex review cycle:

1. **Before committing**, spawn a general-purpose Agent to review the staged diff. The review prompt must include:
   - Read all changed files
   - Check against DESIGN.md architectural requirements
   - Look for: correctness, race conditions, error handling, Rust idiom, test coverage
   - Format as: 主要问题 (numbered, with file:line) / 次要问题 / 已做好的点

2. **Show the review to the user** — paste the full review output.

3. **Fix issues** the user agrees should be fixed. Iterate:
   - Fix → re-run `cargo fmt && cargo clippy --workspace -- -D warnings && cargo test --workspace`
   - If needed, re-review with codex
   - Repeat until review passes or remaining issues are explicitly deferred

4. **Only then commit**.

## Build Commands

```
make check    # cargo clippy + cargo test
make run      # RUST_LOG + cargo run (needs FEISHU_APP_ID/APP_SECRET)
cargo fmt --all -- --check   # format check
```

## Architecture

Three decoupled layers: **Frontend → Agent → Backend**

- Agent (`src/agent.rs`): task scheduler, per-chat FIFO, two-phase execution
- Frontend (`src/frontend.rs`): Feishu WS inbound + REST outbound rendering
- Backend (`feishu/`): Feishu SDK crate (WS + REST + auth)

Import direction: Frontend imports Agent types. Agent never imports Frontend.

## Shutdown Order

1. Agent stops first (finishes in-flight task, closes events channels)
2. Frontend stops second (drains outbound queue, then exits)

## Language

- Code, comments, commit messages: English
- Conversational replies to user: Chinese
