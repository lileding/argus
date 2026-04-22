-- Redesign: split messages into messages (user→agent) + notifications (agent→user).
-- Merge chat_id + source_im into channel. Drop unused columns.

-- 1. Create notifications table.
CREATE TABLE IF NOT EXISTS notifications (
  id          BIGSERIAL PRIMARY KEY,
  message_id  BIGINT REFERENCES messages(id),
  content     TEXT NOT NULL,
  embedding   vector(768),
  summary     TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 2. Migrate assistant rows → notifications.
--    For each assistant row, find the nearest preceding user row in the same chat.
INSERT INTO notifications (message_id, content, created_at)
SELECT
  (SELECT MAX(id) FROM messages WHERE role = 'user' AND chat_id = a.chat_id AND id < a.id),
  a.content,
  a.created_at
FROM messages a
WHERE a.role = 'assistant';

-- 3. Add reply_id on messages, back-fill from notifications.
ALTER TABLE messages ADD COLUMN IF NOT EXISTS reply_id BIGINT REFERENCES notifications(id);

UPDATE messages m
SET reply_id = n.id
FROM notifications n
WHERE n.message_id = m.id;

-- 4. Add ready column, derive from old reply_status.
ALTER TABLE messages ADD COLUMN IF NOT EXISTS ready BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE messages SET ready = TRUE
WHERE reply_status IN ('ready', 'processing', 'done', 'replied');

-- 5. Merge chat_id + source_im into channel.
--    Old channel was often empty; use source_im:chat_id as canonical form.
UPDATE messages SET channel = source_im || ':' || chat_id
WHERE channel = '' OR channel = chat_id;

-- 6. Delete assistant and tool rows (migrated / will be in traces).
DELETE FROM messages WHERE role IN ('assistant', 'tool');

-- 7. Drop obsolete columns.
ALTER TABLE messages
  DROP COLUMN IF EXISTS role,
  DROP COLUMN IF EXISTS chat_id,
  DROP COLUMN IF EXISTS source_im,
  DROP COLUMN IF EXISTS tool_name,
  DROP COLUMN IF EXISTS tool_call_id,
  DROP COLUMN IF EXISTS reply_status,
  DROP COLUMN IF EXISTS reply_channel_id,
  DROP COLUMN IF EXISTS reply_content,
  DROP COLUMN IF EXISTS summary;

-- 8. New indexes.
CREATE INDEX IF NOT EXISTS idx_notifications_message
  ON notifications (message_id);
CREATE INDEX IF NOT EXISTS idx_notifications_embedding
  ON notifications USING ivfflat (embedding vector_cosine_ops) WITH (lists = 100);
CREATE INDEX IF NOT EXISTS idx_messages_channel_created
  ON messages (channel, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_not_ready
  ON messages (channel, created_at) WHERE NOT ready;

-- 9. Drop obsolete indexes.
DROP INDEX IF EXISTS idx_messages_chat_created;
DROP INDEX IF EXISTS idx_messages_channel;
DROP INDEX IF EXISTS idx_messages_unembedded;
DROP INDEX IF EXISTS idx_messages_reply_queue;
DROP INDEX IF EXISTS idx_messages_unsummarized;
