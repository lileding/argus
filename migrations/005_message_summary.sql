-- Add summary column for long assistant replies.
-- Populated asynchronously by the summarization worker.
ALTER TABLE messages ADD COLUMN IF NOT EXISTS summary TEXT;

CREATE INDEX IF NOT EXISTS idx_messages_unsummarized
  ON messages (id) WHERE summary IS NULL AND role = 'assistant' AND LENGTH(content) > 2400;
