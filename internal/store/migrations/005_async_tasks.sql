-- Durable async task execution ledger.
-- The initial implementation exposes task creation/status/cancellation.
-- Workers will claim queued rows via leases in a later wiring step.

CREATE TABLE IF NOT EXISTS tasks (
    id                 BIGSERIAL PRIMARY KEY,
    kind               TEXT NOT NULL,
    source             TEXT NOT NULL,
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

CREATE INDEX IF NOT EXISTS idx_tasks_claim
    ON tasks (priority DESC, created_at)
    WHERE status = 'queued';

CREATE INDEX IF NOT EXISTS idx_tasks_chat_created
    ON tasks (chat_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_tasks_parent
    ON tasks (parent_task_id)
    WHERE parent_task_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS outbox_events (
    id          BIGSERIAL PRIMARY KEY,
    chat_id     TEXT NOT NULL,
    task_id     BIGINT REFERENCES tasks(id),
    kind        TEXT NOT NULL,
    payload     JSONB NOT NULL DEFAULT '{}'::jsonb,
    status      TEXT NOT NULL DEFAULT 'pending',
    priority    INT NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at     TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_outbox_pending
    ON outbox_events (chat_id, priority DESC, created_at)
    WHERE status = 'pending';
