-- Trace system: full observability for each message processing.
-- One trace per user message → reply cycle; tool_calls are individual steps.

CREATE TABLE IF NOT EXISTS traces (
    id                       BIGSERIAL PRIMARY KEY,
    message_id               BIGINT NOT NULL,       -- user message (messages.id)
    reply_id                 BIGINT,                 -- assistant reply (messages.id, set after processing)
    chat_id                  TEXT NOT NULL,
    iterations               INT NOT NULL DEFAULT 0, -- orchestrator loop count
    summary                  TEXT,                    -- finish_task summary
    total_prompt_tokens      INT NOT NULL DEFAULT 0,
    total_completion_tokens  INT NOT NULL DEFAULT 0,
    synth_prompt_tokens      INT NOT NULL DEFAULT 0,
    synth_completion_tokens  INT NOT NULL DEFAULT 0,
    duration_ms              INT,                     -- total processing time
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_traces_message ON traces (message_id);
CREATE INDEX IF NOT EXISTS idx_traces_chat    ON traces (chat_id, created_at DESC);

CREATE TABLE IF NOT EXISTS tool_calls (
    id            BIGSERIAL PRIMARY KEY,
    trace_id      BIGINT NOT NULL REFERENCES traces(id) ON DELETE CASCADE,
    iteration     INT NOT NULL,           -- orchestrator iteration (0-based)
    seq           INT NOT NULL DEFAULT 0, -- sequence within iteration (for parallel calls)
    tool_name     TEXT NOT NULL,
    arguments     TEXT NOT NULL DEFAULT '',
    result        TEXT,
    is_error      BOOLEAN NOT NULL DEFAULT FALSE,
    duration_ms   INT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_tool_calls_trace ON tool_calls (trace_id, iteration, seq);
