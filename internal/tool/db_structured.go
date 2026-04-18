package tool

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
)

// StructuredDBTool is the single "db" tool that replaces db.go, db_exec.go,
// and sqlsandbox. The model sends CLI-style commands with JSON values;
// all SQL is generated internally with parameterized queries.
type StructuredDBTool struct {
	executor *DBExecutor
}

func NewStructuredDBTool(db *sql.DB) *StructuredDBTool {
	return &StructuredDBTool{executor: NewDBExecutor(db)}
}

func (t *StructuredDBTool) Name() string { return "db" }

func (t *StructuredDBTool) Description() string {
	return `Access your structured data tables. Send a command string.

Commands:
  list                              — show all tables
  describe <table>                  — show table columns and types
  create <table> {col: "type", ...} — create table (! = required)
  query <table> [where {...}] [sort <field> [desc]] [limit N]
  count <table> [where {...}] [group_by [...]] [sum [...]] [avg [...]]
  insert <table> {...} or [{...}, ...]
  update <table> <id> {...}

Types: text, number, date, boolean, timestamp, json
Where operators: exact match (default), __gt, __gte, __lt, __lte, __contains, __neq

Examples:
  list
  describe food_log
  create food_log {"meal_date": "date!", "meal_type": "text!", "food_name": "text", "calories": "number"}
  query food_log where {"meal_date": "2026-04-17"} sort created_at desc limit 10
  count food_log where {"meal_date__gte": "2026-04-16"} group_by ["meal_date"] sum ["calories"]
  insert food_log {"meal_date": "2026-04-17", "meal_type": "lunch", "food_name": "牛排", "calories": 450}
  update food_log 42 {"calories": 550}

Notes:
  - Every table has auto columns: id, created_at, updated_at
  - query default: sort created_at desc, limit 50
  - update only works by id (one row at a time)
  - delete is not available`
}

func (t *StructuredDBTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "The command to execute (see tool description for syntax)"}
		},
		"required": ["command"]
	}`)
}

type dbStructuredArgs struct {
	Command string `json:"command"`
}

func (t *StructuredDBTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args dbStructuredArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("Error: invalid arguments\nExample: {\"command\": \"list\"}")
	}

	slog.Info("db command", "command", args.Command)

	// Parse.
	cmd, err := ParseDBCommand(args.Command)
	if err != nil {
		return "", err
	}

	// Validate columns for verbs that reference them.
	if cmd.Verb == "query" || cmd.Verb == "count" || cmd.Verb == "insert" || cmd.Verb == "update" {
		var fieldNames []string
		for _, f := range cmd.Where {
			fieldNames = append(fieldNames, f.Field)
		}
		for _, s := range cmd.Sort {
			fieldNames = append(fieldNames, s.Field)
		}
		fieldNames = append(fieldNames, cmd.GroupBy...)
		for _, fields := range cmd.Aggregates {
			fieldNames = append(fieldNames, fields...)
		}
		if cmd.Data != nil {
			for k := range cmd.Data {
				fieldNames = append(fieldNames, k)
			}
		}
		for _, row := range cmd.Rows {
			for k := range row {
				fieldNames = append(fieldNames, k)
			}
		}
		if len(fieldNames) > 0 {
			if err := t.executor.ValidateColumns(ctx, cmd.Table, fieldNames); err != nil {
				return "", err
			}
		}
	}

	// Execute.
	result, err := t.executor.Execute(ctx, cmd)
	if err != nil {
		return "", err
	}

	return result, nil
}

// NormalizedArgs returns the normalized command representation for trace logging.
// Called externally by the trace collector after Execute.
func (t *StructuredDBTool) ParseAndNormalize(command string) string {
	cmd, err := ParseDBCommand(command)
	if err != nil {
		return command // fallback to raw
	}
	return cmd.Normalize()
}
