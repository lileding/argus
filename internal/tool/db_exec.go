package tool

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"argus/internal/sqlsandbox"
)

// DBExecTool executes write SQL statements (INSERT, UPDATE, DELETE, CREATE
// TABLE, etc.), rewriting every table reference through sqlsandbox to
// confine the model to its own namespace. System tables (messages,
// memories, documents, chunks, schema_migrations) are unreachable because
// the prefix reserves a disjoint namespace.
type DBExecTool struct {
	db     *sql.DB
	prefix string
}

func NewDBExecTool(db *sql.DB) *DBExecTool {
	return &DBExecTool{db: db, prefix: sqlsandbox.DefaultPrefix}
}

func (t *DBExecTool) Name() string { return "db_exec" }

func (t *DBExecTool) Description() string {
	return "Execute a write SQL statement (INSERT, UPDATE, DELETE, CREATE TABLE, " +
		"ALTER TABLE, CREATE INDEX) against your scratch database. Returns rows " +
		"affected. Use the 'db' tool for SELECT queries. Only one statement per call."
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

func (t *DBExecTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args dbExecArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}

	rewritten, err := sqlsandbox.Rewrite(args.SQL, t.prefix)
	if err != nil {
		return "", err
	}

	result, err := t.db.ExecContext(ctx, rewritten)
	if err != nil {
		errMsg := sqlsandbox.StripPrefix(err.Error(), t.prefix)
		hint := schemaHintForExec(ctx, t.db, rewritten, t.prefix)
		if hint != "" {
			errMsg += "\n\n" + hint
		}
		return "", fmt.Errorf("execute sql: %s", errMsg)
	}

	rowsAffected, _ := result.RowsAffected()
	return fmt.Sprintf("OK, %d rows affected", rowsAffected), nil
}

// schemaHintForExec reuses the same hint logic as DBTool. Extracted as a
// function rather than a method since DBExecTool is a different type.
func schemaHintForExec(ctx context.Context, db *sql.DB, rewrittenSQL, prefix string) string {
	tableName := extractFirstTable(rewrittenSQL, prefix)
	if tableName == "" {
		return ""
	}
	row := db.QueryRowContext(ctx,
		`SELECT string_agg(column_name || ' ' || data_type, ', ' ORDER BY ordinal_position)
		 FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = $1`,
		tableName)
	var cols string
	if err := row.Scan(&cols); err != nil || cols == "" {
		return ""
	}
	visible := sqlsandbox.StripPrefix(tableName, prefix)
	return fmt.Sprintf("Hint: table %q has columns: %s", visible, cols)
}

// extractFirstTable is defined in db.go (same package).
