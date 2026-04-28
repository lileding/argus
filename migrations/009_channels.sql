-- Channel system: channels are the tenant unit. Sinks are IM endpoints
-- (DM / group) and map many-to-one to channels. NULL channel_id = default
-- channel (implicit, unconfigured, always exists).
--
-- This migration is structural: existing rows all become NULL channel_id
-- (default channel) and their existing channel string is migrated to the
-- new sink column where applicable.

CREATE TABLE channels (
    id    BIGSERIAL PRIMARY KEY,
    name  TEXT NOT NULL UNIQUE,
    sinks JSONB NOT NULL DEFAULT '[]'
);

-- The conversation view depends on messages.channel; drop and recreate
-- after the schema change.
DROP VIEW IF EXISTS conversation;

-- messages: channel TEXT -> channel_id BIGINT NULL + sink TEXT NOT NULL
ALTER TABLE messages
    ADD COLUMN channel_id BIGINT REFERENCES channels(id),
    ADD COLUMN sink TEXT NOT NULL DEFAULT '';

UPDATE messages SET sink = channel WHERE sink = '';

-- Drop old indexes that reference the channel column.
DROP INDEX IF EXISTS idx_messages_channel_created;
DROP INDEX IF EXISTS idx_messages_not_ready;

ALTER TABLE messages DROP COLUMN channel;

-- Recreate the conversation view with channel_id instead of channel.
CREATE VIEW conversation AS
SELECT
    m.id,
    m.channel_id,
    m.content       AS user_content,
    m.embedding     AS user_embedding,
    m.created_at    AS user_ts,
    n.content       AS reply_content,
    n.summary       AS reply_summary,
    n.created_at    AS reply_ts
FROM messages m
LEFT JOIN notifications n ON n.id = m.reply_id
WHERE m.ready = TRUE;

CREATE INDEX idx_messages_channel_id_created
    ON messages(channel_id, created_at DESC);
CREATE INDEX idx_messages_not_ready
    ON messages(channel_id, created_at) WHERE NOT ready;

-- crons: channel TEXT -> channel_id BIGINT NULL + sink TEXT NOT NULL
ALTER TABLE crons
    ADD COLUMN channel_id BIGINT REFERENCES channels(id),
    ADD COLUMN sink TEXT NOT NULL DEFAULT '';

UPDATE crons SET sink = channel WHERE sink = '';

DROP INDEX IF EXISTS crons_enabled_channel_idx;
ALTER TABLE crons DROP COLUMN channel;

CREATE INDEX crons_enabled_channel_id_idx ON crons(enabled, channel_id);

-- documents: channel TEXT NULL -> channel_id BIGINT NULL
ALTER TABLE documents
    ADD COLUMN channel_id BIGINT REFERENCES channels(id);
-- (No data migration: old channel value was the sink string; documents
--  don't need a sink, only channel_id, and existing rows go to default.)
ALTER TABLE documents DROP COLUMN channel;

-- traces: chat_id TEXT -> channel_id BIGINT NULL
ALTER TABLE traces
    ADD COLUMN channel_id BIGINT REFERENCES channels(id);

DROP INDEX IF EXISTS idx_traces_chat;
ALTER TABLE traces DROP COLUMN chat_id;

CREATE INDEX idx_traces_channel_id_created
    ON traces(channel_id, created_at DESC);

-- memories: was completely global, now scoped by channel_id (NULL = default).
ALTER TABLE memories
    ADD COLUMN channel_id BIGINT REFERENCES channels(id);
