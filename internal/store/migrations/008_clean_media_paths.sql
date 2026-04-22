-- Clean up media metadata in content and file_paths.
-- 1. Audio: extract transcript from content, move file path to file_paths.
-- 2. All file_paths: strip ".files/" prefix.
-- 3. File messages: remove path details from content.
-- 4. Post messages: remove "(images saved at: ...)" suffix from content.
-- 5. Re-embed affected rows by clearing their embeddings.

-- 1a. Audio: extract file path → file_paths (strip .files/ prefix).
UPDATE messages
SET file_paths = ARRAY[
    regexp_replace(
        (regexp_match(content, '\[Voice message, \d+s, saved at \.files/([^\]]+)\]'))[1],
        '^\.files/', ''
    )
]
WHERE msg_type = 'audio'
  AND content LIKE '[Voice message,%'
  AND (file_paths IS NULL OR file_paths = '{}');

-- 1b. Audio: strip metadata prefix, keep only transcript text.
UPDATE messages
SET content = regexp_replace(content, '^\[Voice message, \d+s, saved at [^\]]+\]\n?', '')
WHERE msg_type = 'audio'
  AND content LIKE '[Voice message,%';

-- 2. Strip ".files/" prefix from all file_paths entries.
UPDATE messages
SET file_paths = (
    SELECT ARRAY_AGG(regexp_replace(p, '^\.files/', ''))
    FROM unnest(file_paths) AS p
)
WHERE file_paths IS NOT NULL
  AND file_paths != '{}'
  AND EXISTS (SELECT 1 FROM unnest(file_paths) AS p WHERE p LIKE '.files/%');

-- 3. File messages: simplify content to just the filename.
UPDATE messages
SET content = regexp_replace(
    content,
    E'^The user sent a file ''([^'']+)'' \\(saved at [^)]+\\)\\.',
    E'The user sent a file ''\\1''.'
)
WHERE msg_type = 'file'
  AND content LIKE 'The user sent a file%saved at%';

-- 4. Post messages: remove "(images saved at: ...)" suffix.
UPDATE messages
SET content = regexp_replace(content, E' \\(images saved at: [^)]+\\)$', '')
WHERE msg_type = 'post'
  AND content LIKE '%(images saved at:%';

-- 5. Clear embeddings for all affected rows so the embedder re-embeds with clean content.
UPDATE messages SET embedding = NULL
WHERE msg_type IN ('audio', 'file', 'post');

-- 6. Refresh the conversation view (it depends on the underlying data).
-- VIEW is auto-refreshed on query, no action needed.
