-- Memory system: pgvector + extended messages + memories + documents + chunks
CREATE EXTENSION IF NOT EXISTS vector;

-- Extend messages table with metadata and embedding
ALTER TABLE messages
  ADD COLUMN IF NOT EXISTS source_im  TEXT NOT NULL DEFAULT 'unknown',
  ADD COLUMN IF NOT EXISTS channel    TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS source_ts  TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS msg_type   TEXT NOT NULL DEFAULT 'text',
  ADD COLUMN IF NOT EXISTS file_paths TEXT[],
  ADD COLUMN IF NOT EXISTS sender_id  TEXT,
  ADD COLUMN IF NOT EXISTS embedding  vector(768);

CREATE INDEX IF NOT EXISTS idx_messages_embedding
  ON messages USING ivfflat (embedding vector_cosine_ops) WITH (lists = 100);
CREATE INDEX IF NOT EXISTS idx_messages_channel
  ON messages (source_im, channel, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_unembedded
  ON messages (id) WHERE embedding IS NULL AND role IN ('user', 'assistant');

-- Pinned memories (user-defined persistent notes)
CREATE TABLE IF NOT EXISTS memories (
  id         BIGSERIAL PRIMARY KEY,
  content    TEXT NOT NULL,
  category   TEXT NOT NULL DEFAULT 'general',
  embedding  vector(768),
  active     BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_memories_embedding
  ON memories USING ivfflat (embedding vector_cosine_ops) WITH (lists = 20);

-- Documents for RAG
CREATE TABLE IF NOT EXISTS documents (
  id         BIGSERIAL PRIMARY KEY,
  filename   TEXT NOT NULL,
  file_path  TEXT NOT NULL,
  channel    TEXT,
  status     TEXT NOT NULL DEFAULT 'pending',
  error_msg  TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Document chunks for RAG retrieval
CREATE TABLE IF NOT EXISTS chunks (
  id          BIGSERIAL PRIMARY KEY,
  document_id BIGINT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  chunk_index INT NOT NULL,
  content     TEXT NOT NULL,
  embedding   vector(768),
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_chunks_embedding
  ON chunks USING ivfflat (embedding vector_cosine_ops) WITH (lists = 100);
CREATE INDEX IF NOT EXISTS idx_chunks_document
  ON chunks (document_id, chunk_index);
