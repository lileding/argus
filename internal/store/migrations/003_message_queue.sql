-- Message queue: turn the messages table into a pipeline state machine.
-- reply_status tracks each user message through: received → filtering → ready → processing → done.
-- Non-user messages (assistant, tool) keep reply_status = NULL and are not part of the queue.

ALTER TABLE messages
    ADD COLUMN IF NOT EXISTS reply_status     TEXT,
    ADD COLUMN IF NOT EXISTS reply_channel_id TEXT,
    ADD COLUMN IF NOT EXISTS trigger_msg_id   TEXT;

-- Partial index: only covers active queue rows (non-NULL, non-done).
-- Keeps the index small and fast even with millions of historical messages.
CREATE INDEX IF NOT EXISTS idx_messages_reply_queue
    ON messages (chat_id, reply_status, created_at)
    WHERE reply_status IS NOT NULL AND reply_status != 'done';
