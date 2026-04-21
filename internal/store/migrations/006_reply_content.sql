-- Store the agent's reply text on the same message row.
ALTER TABLE messages ADD COLUMN IF NOT EXISTS reply_content TEXT;
