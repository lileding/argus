-- Store the agent's reply on the same row as the user message.
-- The role column is no longer used (user messages only; reply in reply_content).

-- 1. Add reply_content column.
ALTER TABLE messages ADD COLUMN IF NOT EXISTS reply_content TEXT;

-- 2. Migrate assistant replies back to their corresponding user message.
--    For each user row, find the nearest following assistant row in the same chat.
UPDATE messages u
SET reply_content = a.content
FROM messages a
WHERE a.role = 'assistant'
  AND a.chat_id = u.chat_id
  AND u.role = 'user'
  AND u.reply_content IS NULL
  AND a.id = (
    SELECT MIN(id) FROM messages
    WHERE role = 'assistant' AND chat_id = u.chat_id AND id > u.id
  );

-- 3. Delete assistant rows (content now lives in reply_content).
DELETE FROM messages WHERE role = 'assistant';
