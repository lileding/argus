package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
	pgvector "github.com/pgvector/pgvector-go"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// PostgresStore implements Store, SemanticStore, PinnedMemoryStore, and DocumentStore.
type PostgresStore struct {
	db *sql.DB
}

func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// --- Migrations ---

func (s *PostgresStore) Migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`)
	if err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version := entry.Name()

		var exists bool
		if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)", version).Scan(&exists); err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if exists {
			continue
		}

		data, err := migrationsFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %s: %w", version, err)
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin tx for %s: %w", version, err)
		}

		if _, err := tx.ExecContext(ctx, string(data)); err != nil {
			tx.Rollback()
			return fmt.Errorf("execute migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", version, err)
		}

		slog.Info("migration applied", "version", version)
	}

	return nil
}

// --- Store interface (base) ---

func (s *PostgresStore) SaveMessage(ctx context.Context, msg *StoredMessage) error {
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO messages (chat_id, role, content, tool_name, tool_call_id,
			source_im, channel, source_ts, msg_type, file_paths, sender_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id, created_at
	`, msg.ChatID, msg.Role, msg.Content, msg.ToolName, msg.ToolCallID,
		msg.SourceIM, msg.Channel, msg.SourceTS, msg.MsgType,
		pq.Array(msg.FilePaths), msg.SenderID,
	).Scan(&msg.ID, &msg.CreatedAt)
	if err != nil {
		return fmt.Errorf("save message: %w", err)
	}
	return nil
}

func (s *PostgresStore) RecentMessages(ctx context.Context, chatID string, limit int) ([]StoredMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, chat_id, role, content, tool_name, tool_call_id,
			source_im, channel, source_ts, msg_type, file_paths, sender_id, created_at
		FROM messages
		WHERE chat_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, chatID, limit)
	if err != nil {
		return nil, fmt.Errorf("query recent messages: %w", err)
	}
	defer rows.Close()

	var messages []StoredMessage
	for rows.Next() {
		var m StoredMessage
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &m.ToolName, &m.ToolCallID,
			&m.SourceIM, &m.Channel, &m.SourceTS, &m.MsgType, pq.Array(&m.FilePaths), &m.SenderID, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		messages = append(messages, m)
	}

	// Reverse to chronological order.
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}

	return messages, nil
}

// --- SemanticStore ---

func (s *PostgresStore) SearchMessages(ctx context.Context, embedding []float32, chatID string, limit int) ([]StoredMessage, error) {
	vec := pgvector.NewVector(embedding)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, chat_id, role, content, source_im, channel, msg_type, created_at,
			embedding <=> $1 AS distance
		FROM messages
		WHERE chat_id = $2 AND embedding IS NOT NULL
		ORDER BY embedding <=> $1
		LIMIT $3
	`, vec, chatID, limit)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	defer rows.Close()

	var results []StoredMessage
	for rows.Next() {
		var m StoredMessage
		var dist float64
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &m.SourceIM, &m.Channel, &m.MsgType, &m.CreatedAt, &dist); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		results = append(results, m)
	}
	return results, nil
}

func (s *PostgresStore) UnembeddedMessages(ctx context.Context, limit int) ([]StoredMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, content
		FROM messages
		WHERE embedding IS NULL AND role IN ('user', 'assistant') AND content != ''
		ORDER BY id
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("query unembedded messages: %w", err)
	}
	defer rows.Close()

	var msgs []StoredMessage
	for rows.Next() {
		var m StoredMessage
		if err := rows.Scan(&m.ID, &m.Content); err != nil {
			return nil, fmt.Errorf("scan unembedded: %w", err)
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}

func (s *PostgresStore) SetMessageEmbedding(ctx context.Context, messageID int64, embedding []float32) error {
	vec := pgvector.NewVector(embedding)
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET embedding = $1 WHERE id = $2`, vec, messageID)
	return err
}

// --- PinnedMemoryStore ---

func (s *PostgresStore) SaveMemory(ctx context.Context, mem *Memory) error {
	return s.db.QueryRowContext(ctx, `
		INSERT INTO memories (content, category) VALUES ($1, $2)
		RETURNING id, created_at
	`, mem.Content, mem.Category).Scan(&mem.ID, &mem.CreatedAt)
}

func (s *PostgresStore) ListMemories(ctx context.Context, activeOnly bool) ([]Memory, error) {
	query := `SELECT id, content, category, active, created_at, updated_at FROM memories`
	if activeOnly {
		query += ` WHERE active = TRUE`
	}
	query += ` ORDER BY created_at DESC`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var mems []Memory
	for rows.Next() {
		var m Memory
		if err := rows.Scan(&m.ID, &m.Content, &m.Category, &m.Active, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		mems = append(mems, m)
	}
	return mems, nil
}

func (s *PostgresStore) SearchMemories(ctx context.Context, embedding []float32, limit int) ([]Memory, error) {
	vec := pgvector.NewVector(embedding)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, content, category, active, created_at
		FROM memories
		WHERE active = TRUE AND embedding IS NOT NULL
		ORDER BY embedding <=> $1
		LIMIT $2
	`, vec, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var mems []Memory
	for rows.Next() {
		var m Memory
		if err := rows.Scan(&m.ID, &m.Content, &m.Category, &m.Active, &m.CreatedAt); err != nil {
			return nil, err
		}
		mems = append(mems, m)
	}
	return mems, nil
}

func (s *PostgresStore) DeleteMemory(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE memories SET active = FALSE, updated_at = NOW() WHERE id = $1`, id)
	return err
}

func (s *PostgresStore) SetMemoryEmbedding(ctx context.Context, memoryID int64, embedding []float32) error {
	vec := pgvector.NewVector(embedding)
	_, err := s.db.ExecContext(ctx, `UPDATE memories SET embedding = $1 WHERE id = $2`, vec, memoryID)
	return err
}

// --- DocumentStore ---

func (s *PostgresStore) SaveDocument(ctx context.Context, doc *Document) error {
	return s.db.QueryRowContext(ctx, `
		INSERT INTO documents (filename, file_path, channel, status)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at
	`, doc.Filename, doc.FilePath, doc.Channel, doc.Status).Scan(&doc.ID, &doc.CreatedAt)
}

func (s *PostgresStore) UpdateDocumentStatus(ctx context.Context, id int64, status, errorMsg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE documents SET status = $1, error_msg = $2 WHERE id = $3`, status, errorMsg, id)
	return err
}

func (s *PostgresStore) PendingDocuments(ctx context.Context, limit int) ([]Document, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, filename, file_path, channel, status, created_at
		FROM documents WHERE status = 'pending' ORDER BY created_at LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var docs []Document
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.ID, &d.Filename, &d.FilePath, &d.Channel, &d.Status, &d.CreatedAt); err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	return docs, nil
}

func (s *PostgresStore) SaveChunks(ctx context.Context, chunks []Chunk) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, c := range chunks {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO chunks (document_id, chunk_index, content) VALUES ($1, $2, $3)
		`, c.DocumentID, c.ChunkIndex, c.Content); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *PostgresStore) SearchChunks(ctx context.Context, embedding []float32, limit int) ([]Chunk, error) {
	vec := pgvector.NewVector(embedding)
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.document_id, c.chunk_index, c.content,
			d.filename, 1 - (c.embedding <=> $1) AS similarity
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE c.embedding IS NOT NULL AND d.status = 'ready'
		ORDER BY c.embedding <=> $1
		LIMIT $2
	`, vec, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chunks []Chunk
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.ID, &c.DocumentID, &c.ChunkIndex, &c.Content, &c.DocFilename, &c.Similarity); err != nil {
			return nil, err
		}
		chunks = append(chunks, c)
	}
	return chunks, nil
}

func (s *PostgresStore) ListDocuments(ctx context.Context) ([]Document, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.id, d.filename, d.file_path, d.status, d.error_msg, d.created_at,
			(SELECT COUNT(*) FROM chunks c WHERE c.document_id = d.id) AS chunk_count
		FROM documents d
		ORDER BY d.created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var docs []Document
	for rows.Next() {
		var d Document
		var chunkCount int
		var errorMsg sql.NullString
		if err := rows.Scan(&d.ID, &d.Filename, &d.FilePath, &d.Status, &errorMsg, &d.CreatedAt, &chunkCount); err != nil {
			return nil, err
		}
		if errorMsg.Valid {
			d.ErrorMsg = errorMsg.String
		}
		d.ChunkCount = chunkCount
		docs = append(docs, d)
	}
	return docs, nil
}

func (s *PostgresStore) UnembeddedChunks(ctx context.Context, limit int) ([]Chunk, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, content FROM chunks WHERE embedding IS NULL ORDER BY id LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chunks []Chunk
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.ID, &c.Content); err != nil {
			return nil, err
		}
		chunks = append(chunks, c)
	}
	return chunks, nil
}

func (s *PostgresStore) SetChunkEmbedding(ctx context.Context, chunkID int64, embedding []float32) error {
	vec := pgvector.NewVector(embedding)
	_, err := s.db.ExecContext(ctx, `UPDATE chunks SET embedding = $1 WHERE id = $2`, vec, chunkID)
	return err
}

// --- RepairableStore ---

// RepairStuckDocuments resets documents stuck in "processing" back to "pending"
// so the ingester can retry them. This happens when the program crashes mid-ingestion.
func (s *PostgresStore) RepairStuckDocuments(ctx context.Context) (int, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE documents SET status = 'pending', error_msg = 'reset: was stuck in processing'
		WHERE status = 'processing'
	`)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// RepairOrphanChunks deletes chunks belonging to documents that are not "ready",
// so they can be re-ingested cleanly.
func (s *PostgresStore) RepairOrphanChunks(ctx context.Context) (int, error) {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM chunks WHERE document_id IN (
			SELECT id FROM documents WHERE status != 'ready'
		)
	`)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// CountUnembeddedMessages returns the number of messages still needing embedding.
func (s *PostgresStore) CountUnembeddedMessages(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM messages WHERE embedding IS NULL AND role IN ('user', 'assistant') AND content != ''
	`).Scan(&count)
	return count, err
}

// FailedTranscriptions returns audio messages where transcription failed.
func (s *PostgresStore) FailedTranscriptions(ctx context.Context) ([]StoredMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, chat_id, content, file_paths
		FROM messages
		WHERE msg_type = 'audio' AND content LIKE '%transcription failed%'
		ORDER BY created_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []StoredMessage
	for rows.Next() {
		var m StoredMessage
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Content, pq.Array(&m.FilePaths)); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}

// --- TraceStore ---

func (s *PostgresStore) CreateTrace(ctx context.Context, t *Trace) error {
	return s.db.QueryRowContext(ctx, `
		INSERT INTO traces (message_id, chat_id, orchestrator_model, synthesizer_model, task_id, parent_task_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at
	`, t.MessageID, t.ChatID, t.OrchestratorModel, t.SynthesizerModel, t.TaskID, t.ParentTaskID).Scan(&t.ID, &t.CreatedAt)
}

func (s *PostgresStore) FinishTrace(ctx context.Context, t *Trace) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE traces SET
			reply_id = $1, iterations = $2, summary = $3,
			total_prompt_tokens = $4, total_completion_tokens = $5,
			synth_prompt_tokens = $6, synth_completion_tokens = $7,
			duration_ms = $8
		WHERE id = $9
	`, t.ReplyID, t.Iterations, t.Summary,
		t.TotalPromptTokens, t.TotalCompletionTokens,
		t.SynthPromptTokens, t.SynthCompletionTokens,
		t.DurationMs, t.ID)
	return err
}

func (s *PostgresStore) SaveToolCalls(ctx context.Context, calls []ToolCallRecord) error {
	if len(calls) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, c := range calls {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tool_calls (trace_id, iteration, seq, tool_name, arguments, normalized_args, result, is_error, duration_ms)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`, c.TraceID, c.Iteration, c.Seq, c.ToolName, c.Arguments, c.NormalizedArgs, c.Result, c.IsError, c.DurationMs); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// --- TaskStore ---

func (s *PostgresStore) CreateTask(ctx context.Context, task *Task) error {
	if task.Kind == "" {
		task.Kind = "async"
	}
	if task.Source == "" {
		task.Source = "agent"
	}
	if task.Status == "" {
		task.Status = "queued"
	}
	if len(task.Input) == 0 {
		task.Input = []byte(`{}`)
	}

	err := s.db.QueryRowContext(ctx, `
		INSERT INTO tasks (
			kind, source, chat_id, user_id, parent_task_id, trigger_message_id,
			status, priority, title, input
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)
		RETURNING id, created_at
	`, task.Kind, task.Source, task.ChatID, task.UserID, task.ParentTaskID,
		task.TriggerMessageID, task.Status, task.Priority, task.Title, string(task.Input),
	).Scan(&task.ID, &task.CreatedAt)
	if err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetTask(ctx context.Context, taskID int64) (*Task, error) {
	var t Task
	var parentID, triggerID sql.NullInt64
	var userID, leaseOwner sql.NullString
	var leaseUntil, startedAt, finishedAt sql.NullTime
	var input []byte

	err := s.db.QueryRowContext(ctx, `
		SELECT id, kind, source, chat_id, user_id, parent_task_id,
			trigger_message_id, status, priority, title, input, result, error,
			lease_owner, lease_until, created_at, started_at, finished_at
		FROM tasks
		WHERE id = $1
	`, taskID).Scan(
		&t.ID, &t.Kind, &t.Source, &t.ChatID, &userID, &parentID,
		&triggerID, &t.Status, &t.Priority, &t.Title, &input, &t.Result, &t.Error,
		&leaseOwner, &leaseUntil, &t.CreatedAt, &startedAt, &finishedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	t.Input = input
	t.UserID = userID.String
	if parentID.Valid {
		t.ParentTaskID = &parentID.Int64
	}
	if triggerID.Valid {
		t.TriggerMessageID = &triggerID.Int64
	}
	t.LeaseOwner = leaseOwner.String
	if leaseUntil.Valid {
		t.LeaseUntil = &leaseUntil.Time
	}
	if startedAt.Valid {
		t.StartedAt = &startedAt.Time
	}
	if finishedAt.Valid {
		t.FinishedAt = &finishedAt.Time
	}
	return &t, nil
}

func (s *PostgresStore) ClaimNextTask(ctx context.Context, workerID string, leaseUntil time.Time) (*Task, error) {
	var taskID int64
	err := s.db.QueryRowContext(ctx, `
		UPDATE tasks
		SET status = 'running',
			lease_owner = $1,
			lease_until = $2,
			started_at = COALESCE(started_at, NOW())
		WHERE id = (
			SELECT id FROM tasks
			WHERE status = 'queued'
			ORDER BY priority DESC, created_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id
	`, workerID, leaseUntil).Scan(&taskID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim next task: %w", err)
	}
	return s.GetTask(ctx, taskID)
}

func (s *PostgresStore) CompleteTask(ctx context.Context, taskID int64, result string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE tasks
		SET status = 'succeeded',
			result = $1,
			error = '',
			lease_owner = NULL,
			lease_until = NULL,
			finished_at = NOW()
		WHERE id = $2 AND status = 'running'
	`, result, taskID)
	return err
}

func (s *PostgresStore) FailTask(ctx context.Context, taskID int64, errorMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE tasks
		SET status = 'failed',
			error = $1,
			lease_owner = NULL,
			lease_until = NULL,
			finished_at = NOW()
		WHERE id = $2 AND status = 'running'
	`, errorMsg, taskID)
	return err
}

func (s *PostgresStore) CancelTask(ctx context.Context, taskID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks
		SET status = 'cancelled',
			lease_owner = NULL,
			lease_until = NULL,
			finished_at = NOW()
		WHERE id = $1 AND status IN ('queued', 'running')
	`, taskID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *PostgresStore) RecoverExpiredTasks(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks
		SET status = 'queued',
			lease_owner = NULL,
			lease_until = NULL
		WHERE status = 'running' AND lease_until IS NOT NULL AND lease_until < $1
	`, now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// --- OutboxStore ---

func (s *PostgresStore) CreateOutboxEvent(ctx context.Context, event *OutboxEvent) error {
	if event.Kind == "" {
		event.Kind = "notice"
	}
	if event.Status == "" {
		event.Status = "pending"
	}
	if len(event.Payload) == 0 {
		event.Payload = []byte(`{}`)
	}

	err := s.db.QueryRowContext(ctx, `
		INSERT INTO outbox_events (chat_id, task_id, kind, payload, status, priority)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6)
		RETURNING id, created_at
	`, event.ChatID, event.TaskID, event.Kind, string(event.Payload), event.Status, event.Priority).
		Scan(&event.ID, &event.CreatedAt)
	if err != nil {
		return fmt.Errorf("create outbox event: %w", err)
	}
	return nil
}

func (s *PostgresStore) PendingOutboxChats(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT chat_id
		FROM outbox_events
		WHERE status = 'pending'
		ORDER BY chat_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chats []string
	for rows.Next() {
		var chatID string
		if err := rows.Scan(&chatID); err != nil {
			return nil, err
		}
		chats = append(chats, chatID)
	}
	return chats, nil
}

func (s *PostgresStore) ClaimNextOutboxEvent(ctx context.Context, chatID string) (*OutboxEvent, error) {
	var e OutboxEvent
	var taskID sql.NullInt64
	var sentAt sql.NullTime
	var payload []byte

	err := s.db.QueryRowContext(ctx, `
		UPDATE outbox_events
		SET status = 'sending'
		WHERE id = (
			SELECT id FROM outbox_events
			WHERE chat_id = $1 AND status = 'pending'
			ORDER BY priority DESC, created_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, chat_id, task_id, kind, payload, status, priority, error, created_at, sent_at
	`, chatID).Scan(&e.ID, &e.ChatID, &taskID, &e.Kind, &payload, &e.Status, &e.Priority, &e.Error, &e.CreatedAt, &sentAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim outbox event: %w", err)
	}
	e.Payload = payload
	if taskID.Valid {
		e.TaskID = &taskID.Int64
	}
	if sentAt.Valid {
		e.SentAt = &sentAt.Time
	}
	return &e, nil
}

func (s *PostgresStore) MarkOutboxSent(ctx context.Context, eventID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE outbox_events
		SET status = 'sent', error = '', sent_at = NOW()
		WHERE id = $1
	`, eventID)
	return err
}

func (s *PostgresStore) MarkOutboxError(ctx context.Context, eventID int64, errorMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE outbox_events
		SET status = 'pending', error = $1
		WHERE id = $2
	`, errorMsg, eventID)
	return err
}

func (s *PostgresStore) RecoverOutbox(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE outbox_events
		SET status = 'pending'
		WHERE status = 'sending'
	`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// --- CronStore ---

func (s *PostgresStore) CreateCronSchedule(ctx context.Context, schedule *CronSchedule) error {
	if schedule.ScheduleType == "" {
		schedule.ScheduleType = "daily"
	}
	if schedule.Timezone == "" {
		schedule.Timezone = "Asia/Shanghai"
	}
	schedule.Enabled = true
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO cron_schedules (
			chat_id, user_id, name, schedule_type, cron_expr, hour, minute,
			timezone, prompt, enabled, created_by_task_id, next_run_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id, created_at, updated_at
	`, schedule.ChatID, schedule.UserID, schedule.Name, schedule.ScheduleType,
		nullEmpty(schedule.CronExpr), schedule.Hour, schedule.Minute, schedule.Timezone,
		schedule.Prompt, schedule.Enabled, schedule.CreatedByTaskID, schedule.NextRunAt,
	).Scan(&schedule.ID, &schedule.CreatedAt, &schedule.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create cron schedule: %w", err)
	}
	return nil
}

func (s *PostgresStore) ListCronSchedules(ctx context.Context, chatID string, includeDisabled bool) ([]CronSchedule, error) {
	query := `
		SELECT id, chat_id, user_id, name, schedule_type, cron_expr, hour, minute,
			timezone, prompt, enabled, created_by_task_id, last_run_at, next_run_at,
			created_at, updated_at
		FROM cron_schedules
		WHERE chat_id = $1`
	if !includeDisabled {
		query += ` AND enabled = TRUE`
	}
	query += ` ORDER BY created_at DESC`

	rows, err := s.db.QueryContext(ctx, query, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCronSchedules(rows)
}

func (s *PostgresStore) DeleteCronSchedule(ctx context.Context, scheduleID int64, chatID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE cron_schedules
		SET enabled = FALSE, updated_at = NOW()
		WHERE id = $1 AND chat_id = $2 AND enabled = TRUE
	`, scheduleID, chatID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *PostgresStore) DueCronSchedules(ctx context.Context, now time.Time, limit int) ([]CronSchedule, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, chat_id, user_id, name, schedule_type, cron_expr, hour, minute,
			timezone, prompt, enabled, created_by_task_id, last_run_at, next_run_at,
			created_at, updated_at
		FROM cron_schedules
		WHERE enabled = TRUE AND next_run_at IS NOT NULL AND next_run_at <= $1
		ORDER BY next_run_at, id
		LIMIT $2
	`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCronSchedules(rows)
}

func (s *PostgresStore) MarkCronScheduleRun(ctx context.Context, scheduleID int64, lastRunAt, nextRunAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE cron_schedules
		SET last_run_at = $1, next_run_at = $2, updated_at = NOW()
		WHERE id = $3
	`, lastRunAt, nextRunAt, scheduleID)
	return err
}

func scanCronSchedules(rows *sql.Rows) ([]CronSchedule, error) {
	var schedules []CronSchedule
	for rows.Next() {
		var s CronSchedule
		var userID, cronExpr sql.NullString
		var createdBy sql.NullInt64
		var lastRun, nextRun sql.NullTime
		if err := rows.Scan(&s.ID, &s.ChatID, &userID, &s.Name, &s.ScheduleType, &cronExpr,
			&s.Hour, &s.Minute, &s.Timezone, &s.Prompt, &s.Enabled, &createdBy,
			&lastRun, &nextRun, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		s.UserID = userID.String
		s.CronExpr = cronExpr.String
		if createdBy.Valid {
			s.CreatedByTaskID = &createdBy.Int64
		}
		if lastRun.Valid {
			s.LastRunAt = &lastRun.Time
		}
		if nextRun.Valid {
			s.NextRunAt = &nextRun.Time
		}
		schedules = append(schedules, s)
	}
	return schedules, nil
}

func nullEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --- QueueStore (message pipeline) ---

func (s *PostgresStore) SaveMessageQueued(ctx context.Context, msg *StoredMessage) error {
	status := "received"
	msg.ReplyStatus = &status
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO messages (chat_id, role, content, tool_name, tool_call_id,
			source_im, channel, source_ts, msg_type, file_paths, sender_id,
			reply_status, trigger_msg_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id, created_at
	`, msg.ChatID, msg.Role, msg.Content, msg.ToolName, msg.ToolCallID,
		msg.SourceIM, msg.Channel, msg.SourceTS, msg.MsgType,
		pq.Array(msg.FilePaths), msg.SenderID,
		status, msg.TriggerMsgID,
	).Scan(&msg.ID, &msg.CreatedAt)
	if err != nil {
		return fmt.Errorf("save queued message: %w", err)
	}
	return nil
}

func (s *PostgresStore) UpdateMessageContent(ctx context.Context, msgID int64, content string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET content = $1 WHERE id = $2`, content, msgID)
	return err
}

func (s *PostgresStore) SetReplyStatus(ctx context.Context, msgID int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET reply_status = $1 WHERE id = $2`, status, msgID)
	return err
}

func (s *PostgresStore) UpdateMessageFilePaths(ctx context.Context, msgID int64, paths []string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET file_paths = $1 WHERE id = $2`, pq.Array(paths), msgID)
	return err
}

func (s *PostgresStore) AckReply(ctx context.Context, msgID int64, replyChannelID string) error {
	// Only stores the reply channel ID. Does NOT change reply_status —
	// the caller is responsible for status transitions. Previously this
	// set status='ready' which was correct when called by Filter, but
	// after the refactor the Dispatcher calls it post-claim (status is
	// already 'processing') and resetting to 'ready' was a bug that
	// could cause duplicate claims.
	_, err := s.db.ExecContext(ctx, `
		UPDATE messages SET reply_channel_id = $1 WHERE id = $2
	`, replyChannelID, msgID)
	return err
}

func (s *PostgresStore) ClaimNextReply(ctx context.Context, chatID string) (*StoredMessage, error) {
	var m StoredMessage
	var replyStatus sql.NullString
	var replyChannelID, triggerMsgID sql.NullString
	err := s.db.QueryRowContext(ctx, `
		UPDATE messages SET reply_status = 'processing'
		WHERE id = (
			SELECT id FROM messages
			WHERE chat_id = $1 AND reply_status = 'ready'
			ORDER BY created_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, chat_id, role, content, source_im, channel, msg_type,
			file_paths, sender_id, created_at,
			reply_status, reply_channel_id, trigger_msg_id
	`, chatID).Scan(
		&m.ID, &m.ChatID, &m.Role, &m.Content, &m.SourceIM, &m.Channel, &m.MsgType,
		pq.Array(&m.FilePaths), &m.SenderID, &m.CreatedAt,
		&replyStatus, &replyChannelID, &triggerMsgID,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim next reply: %w", err)
	}
	if replyStatus.Valid {
		m.ReplyStatus = &replyStatus.String
	}
	m.ReplyChannelID = replyChannelID.String
	m.TriggerMsgID = triggerMsgID.String
	return &m, nil
}

func (s *PostgresStore) FinishReply(ctx context.Context, msgID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET reply_status = 'done' WHERE id = $1`, msgID)
	return err
}

func (s *PostgresStore) RecoverQueue(ctx context.Context) (recovered int, unacked []StoredMessage, err error) {
	// processing → ready (crash during agent run)
	res, err := s.db.ExecContext(ctx, `UPDATE messages SET reply_status = 'ready' WHERE reply_status = 'processing'`)
	if err != nil {
		return 0, nil, fmt.Errorf("recover processing: %w", err)
	}
	n1, _ := res.RowsAffected()

	recovered = int(n1)

	// Return received rows for Filter re-processing.
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, chat_id, content, msg_type, source_im, trigger_msg_id, created_at
		FROM messages WHERE reply_status = 'received' ORDER BY created_at
	`)
	if err != nil {
		return recovered, nil, fmt.Errorf("query received: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m StoredMessage
		var triggerMsgID sql.NullString
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Content, &m.MsgType, &m.SourceIM, &triggerMsgID, &m.CreatedAt); err != nil {
			return recovered, nil, fmt.Errorf("scan received: %w", err)
		}
		m.TriggerMsgID = triggerMsgID.String
		unacked = append(unacked, m)
	}
	return recovered, unacked, nil
}

func (s *PostgresStore) PendingChats(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT chat_id FROM messages WHERE reply_status = 'ready'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chats []string
	for rows.Next() {
		var chatID string
		if err := rows.Scan(&chatID); err != nil {
			return nil, err
		}
		chats = append(chats, chatID)
	}
	return chats, nil
}
