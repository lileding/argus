package tool

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"argus/internal/sqlsandbox"
)

// DBTool executes read-only SQL queries against the database, rewriting
// table references through sqlsandbox so the model can only reach its
// own namespace of tables (see internal/sqlsandbox).
type DBTool struct {
	db     *sql.DB
	prefix string
}

func NewDBTool(db *sql.DB) *DBTool {
	return &DBTool{db: db, prefix: sqlsandbox.DefaultPrefix}
}

func (t *DBTool) Name() string { return "db" }

func (t *DBTool) Description() string {
	return "Execute a read-only SQL query against your scratch database. " +
		"You can query any tables you have previously created (e.g. food_log). " +
		"Use standard PostgreSQL syntax. Returns results as a table."
}

func (t *DBTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"sql": {"type": "string", "description": "SQL query to execute (read-only)"}
		},
		"required": ["sql"]
	}`)
}

type dbArgs struct {
	SQL string `json:"sql"`
}

func (t *DBTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args dbArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}

	rewritten, err := sqlsandbox.Rewrite(args.SQL, t.prefix)
	if err != nil {
		return "", err
	}

	// Execute in a read-only transaction for safety.
	tx, err := t.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, rewritten)
	if err != nil {
		return "", fmt.Errorf("execute query: %s", sqlsandbox.StripPrefix(err.Error(), t.prefix))
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return "", fmt.Errorf("get columns: %w", err)
	}

	var sb strings.Builder
	sb.WriteString(strings.Join(cols, "\t"))
	sb.WriteString("\n")

	values := make([]interface{}, len(cols))
	scanArgs := make([]interface{}, len(cols))
	for i := range values {
		scanArgs[i] = &values[i]
	}

	rowCount := 0
	for rows.Next() {
		if err := rows.Scan(scanArgs...); err != nil {
			return "", fmt.Errorf("scan row: %w", err)
		}
		parts := make([]string, len(cols))
		for i, v := range values {
			if v == nil {
				parts[i] = "NULL"
			} else {
				parts[i] = fmt.Sprintf("%v", v)
			}
		}
		sb.WriteString(strings.Join(parts, "\t"))
		sb.WriteString("\n")
		rowCount++

		if rowCount >= 100 {
			sb.WriteString("... (truncated at 100 rows)\n")
			break
		}
	}

	sb.WriteString(fmt.Sprintf("\n(%d rows)", rowCount))
	return sb.String(), nil
}
