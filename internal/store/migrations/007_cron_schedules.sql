-- Database-backed schedules that produce async tasks.

CREATE TABLE IF NOT EXISTS cron_schedules (
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

CREATE INDEX IF NOT EXISTS idx_cron_schedules_due
    ON cron_schedules (next_run_at)
    WHERE enabled = TRUE;

CREATE INDEX IF NOT EXISTS idx_cron_schedules_chat
    ON cron_schedules (chat_id, enabled, created_at DESC);
