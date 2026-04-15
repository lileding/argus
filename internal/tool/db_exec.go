package tool

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// DBExecTool executes write SQL statements (INSERT, UPDATE, DELETE, CREATE TABLE, etc.).
type DBExecTool struct {
	db *sql.DB
}

func NewDBExecTool(db *sql.DB) *DBExecTool {
	return &DBExecTool{db: db}
}

func (t *DBExecTool) Name() string { return "db_exec" }

func (t *DBExecTool) Description() string {
	return "Execute a write SQL statement (INSERT, UPDATE, DELETE, CREATE TABLE, ALTER TABLE) against the database. Returns rows affected. Use the 'db' tool for SELECT queries."
}

func (t *DBExecTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"sql": {"type": "string", "description": "SQL statement to execute"}
		},
		"required": ["sql"]
	}`)
}

type dbExecArgs struct {
	SQL string `json:"sql"`
}

// Protected tables that cannot be dropped.
var protectedTables = []string{"messages", "schema_migrations", "memories", "documents", "chunks"}

func (t *DBExecTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args dbExecArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}

	// Safety check: prevent dropping protected tables.
	sqlLower := strings.ToLower(strings.TrimSpace(args.SQL))
	for _, table := range protectedTables {
		if strings.Contains(sqlLower, "drop") && strings.Contains(sqlLower, table) {
			return "", fmt.Errorf("cannot drop protected table: %s", table)
		}
	}

	result, err := t.db.ExecContext(ctx, args.SQL)
	if err != nil {
		return "", fmt.Errorf("execute sql: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	return fmt.Sprintf("OK, %d rows affected", rowsAffected), nil
}
