-- Cron table: persistent scheduled tasks.
CREATE TABLE crons (
    id          BIGSERIAL PRIMARY KEY,
    cron_expr   TEXT NOT NULL,
    goal        TEXT NOT NULL,
    channel     TEXT NOT NULL,
    msg_id      TEXT NOT NULL,
    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    last_run_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX crons_enabled_channel_idx ON crons(enabled, channel);
