package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// PostgresStore implements Store using PostgreSQL.
type PostgresStore struct {
	db *sql.DB
}

func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

func (s *PostgresStore) Migrate(ctx context.Context) error {
	// Create migrations tracking table.
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`)
	if err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}

	// Read all migration files.
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	// Sort by filename.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		version := entry.Name()

		// Check if already applied.
		var exists bool
		err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)", version).Scan(&exists)
		if err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if exists {
			continue
		}

		// Read and execute migration.
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

func (s *PostgresStore) SaveMessage(ctx context.Context, msg *StoredMessage) error {
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO messages (chat_id, role, content, tool_name, tool_call_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, created_at
	`, msg.ChatID, msg.Role, msg.Content, msg.ToolName, msg.ToolCallID).Scan(&msg.ID, &msg.CreatedAt)
	if err != nil {
		return fmt.Errorf("save message: %w", err)
	}
	return nil
}

func (s *PostgresStore) RecentMessages(ctx context.Context, chatID string, limit int) ([]StoredMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, chat_id, role, content, tool_name, tool_call_id, created_at
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
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &m.ToolName, &m.ToolCallID, &m.CreatedAt); err != nil {
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
