-- Conversation view: joins messages with their notification replies.
-- Used by the agent harness to build model context.
CREATE OR REPLACE VIEW conversation AS
SELECT
    m.id,
    m.channel,
    m.content       AS user_content,
    m.embedding     AS user_embedding,
    m.created_at    AS user_ts,
    n.content       AS reply_content,
    n.summary       AS reply_summary,
    n.created_at    AS reply_ts
FROM messages m
LEFT JOIN notifications n ON n.id = m.reply_id;
