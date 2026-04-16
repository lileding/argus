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
		return "", fmt.Errorf("execute sql: %s", sqlsandbox.StripPrefix(err.Error(), t.prefix))
	}

	rowsAffected, _ := result.RowsAffected()
	return fmt.Sprintf("OK, %d rows affected", rowsAffected), nil
}
