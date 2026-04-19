// Package tool provides the structured db tool that replaces raw SQL access.
// The model writes CLI-style commands with JSON values; the parser, validator,
// and executor handle all SQL generation internally.
package tool

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
)

const dbPrefix = "argus_"
const defaultLimit = 50
const maxLimit = 200

// --- Command types ---

type DBCommand struct {
	Verb       string
	Table      string
	ID         int64
	Where      []DBFilter
	Sort       []DBSort
	Limit      int
	GroupBy    []string
	Aggregates map[string][]string // "sum"→["calories"], "avg"→["price"]
	Data       map[string]any      // single row insert / update fields
	Rows       []map[string]any    // batch insert
	Columns    map[string]string   // create: "meal_date"→"date!"
}

type DBFilter struct {
	Field string `json:"field"`
	Op    string `json:"op"` // eq, gt, gte, lt, lte, contains, neq
	Value any    `json:"value"`
}

type DBSort struct {
	Field string `json:"field"`
	Order string `json:"order"` // asc, desc
}

// Normalize returns a deterministic JSON representation for trace logging.
func (c *DBCommand) Normalize() string {
	norm := map[string]any{"verb": c.Verb}
	if c.Table != "" {
		norm["table"] = c.Table
	}
	if c.ID > 0 {
		norm["id"] = c.ID
	}
	if len(c.Where) > 0 {
		norm["where"] = c.Where
	}
	if len(c.Sort) > 0 {
		norm["sort"] = c.Sort
	}
	if c.Limit > 0 {
		norm["limit"] = c.Limit
	}
	if len(c.GroupBy) > 0 {
		norm["group_by"] = c.GroupBy
	}
	if len(c.Aggregates) > 0 {
		norm["aggregates"] = c.Aggregates
	}
	if c.Data != nil {
		norm["data"] = c.Data
	}
	if len(c.Rows) > 0 {
		norm["rows"] = c.Rows
	}
	if len(c.Columns) > 0 {
		norm["columns"] = c.Columns
	}
	data, _ := json.Marshal(norm)
	return string(data)
}

// --- Parser ---

func ParseDBCommand(input string) (*DBCommand, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, fmt.Errorf("empty command")
	}

	verb, rest := splitFirst(input)
	verb = strings.ToLower(verb)

	switch verb {
	case "list":
		return &DBCommand{Verb: "list"}, nil
	case "describe":
		table, _ := splitFirst(rest)
		if table == "" {
			return nil, parseErr("describe requires a table name", "describe food_log")
		}
		return &DBCommand{Verb: "describe", Table: table}, nil
	case "create":
		return parseCreate(rest)
	case "query":
		return parseQuery(rest)
	case "count":
		return parseCount(rest)
	case "insert":
		return parseInsert(rest)
	case "update":
		return parseUpdate(rest)
	default:
		return nil, parseErr(
			fmt.Sprintf("unknown command %q", verb),
			"list | describe | create | query | count | insert | update",
		)
	}
}

func parseTableName(rest, verb, example string) (string, string, error) {
	table, remainder := splitFirst(rest)
	if table == "" {
		return "", "", parseErr(verb+" requires a table name", example)
	}
	table = strings.ToLower(table)
	if err := validateIdentifier(table, "table"); err != nil {
		return "", "", err
	}
	return table, remainder, nil
}

func parseCreate(rest string) (*DBCommand, error) {
	table, remainder, err := parseTableName(rest, "create", `create food_log {"name": "text!", "value": "number"}`)
	if err != nil {
		return nil, err
	}
	jsonStr := strings.TrimSpace(remainder)
	if jsonStr == "" || jsonStr[0] != '{' {
		return nil, parseErr("create requires a JSON object of column definitions", `create food_log {"name": "text!", "value": "number"}`)
	}
	var cols map[string]string
	if err := json.Unmarshal([]byte(jsonStr), &cols); err != nil {
		return nil, parseErr(fmt.Sprintf("invalid column definitions: %v", err), `create food_log {"name": "text!", "value": "number"}`)
	}
	// Normalize column names to lowercase and validate.
	normalized := make(map[string]string, len(cols))
	for name, typ := range cols {
		lower := strings.ToLower(name)
		if err := validateIdentifier(lower, "column"); err != nil {
			return nil, err
		}
		normalized[lower] = typ
	}
	cols = normalized
	for col, typ := range cols {
		base := strings.TrimSuffix(typ, "!")
		if !isValidType(base) {
			return nil, parseErr(
				fmt.Sprintf("invalid type %q for column %q", base, col),
				"valid types: text, number, date, boolean, timestamp, json",
			)
		}
	}
	return &DBCommand{Verb: "create", Table: table, Columns: cols}, nil
}

func parseQuery(rest string) (*DBCommand, error) {
	table, remainder, err := parseTableName(rest, "query", "query food_log where {...} sort created_at desc limit 20")
	if err != nil {
		return nil, err
	}
	cmd := &DBCommand{Verb: "query", Table: table, Limit: defaultLimit}
	return parseClauses(cmd, remainder)
}

func parseCount(rest string) (*DBCommand, error) {
	table, remainder, err := parseTableName(rest, "count", `count food_log group_by ["meal_date"] sum ["calories"]`)
	if err != nil {
		return nil, err
	}
	cmd := &DBCommand{Verb: "count", Table: table, Aggregates: map[string][]string{}}
	return parseClauses(cmd, remainder)
}

func parseInsert(rest string) (*DBCommand, error) {
	table, remainder, err := parseTableName(rest, "insert", `insert food_log {"name": "test", "value": 42}`)
	if err != nil {
		return nil, err
	}
	jsonStr := strings.TrimSpace(remainder)
	if jsonStr == "" {
		return nil, parseErr("insert requires data", `insert food_log {"name": "test", "value": 42}`)
	}

	cmd := &DBCommand{Verb: "insert", Table: table}
	if jsonStr[0] == '[' {
		var rows []map[string]any
		if err := json.Unmarshal([]byte(jsonStr), &rows); err != nil {
			return nil, parseErr(fmt.Sprintf("invalid data array: %v", err), `insert food_log [{"a": 1}, {"a": 2}]`)
		}
		if len(rows) == 0 {
			return nil, parseErr("empty data array", `insert food_log [{"a": 1}]`)
		}
		cmd.Rows = rows
	} else if jsonStr[0] == '{' {
		var data map[string]any
		if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
			return nil, parseErr(fmt.Sprintf("invalid data object: %v", err), `insert food_log {"a": 1}`)
		}
		cmd.Data = data
	} else {
		return nil, parseErr("insert data must be a JSON object or array", `insert food_log {"a": 1}`)
	}
	return cmd, nil
}

func parseUpdate(rest string) (*DBCommand, error) {
	table, remainder, err := parseTableName(rest, "update", `update food_log 42 {"calories": 550}`)
	if err != nil {
		return nil, err
	}
	idStr, remainder := splitFirst(remainder)
	id := parseInt(idStr)
	if id <= 0 {
		return nil, parseErr("update requires a valid numeric id", `update food_log 42 {"calories": 550}`)
	}
	jsonStr := strings.TrimSpace(remainder)
	if jsonStr == "" || jsonStr[0] != '{' {
		return nil, parseErr("update requires a JSON object of fields to set", `update food_log 42 {"calories": 550}`)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		return nil, parseErr(fmt.Sprintf("invalid update data: %v", err), `update food_log 42 {"calories": 550}`)
	}
	return &DBCommand{Verb: "update", Table: table, ID: id, Data: data}, nil
}

// parseClauses handles where/sort/limit/group_by/sum/avg/min/max clauses.
func parseClauses(cmd *DBCommand, input string) (*DBCommand, error) {
	rest := strings.TrimSpace(input)
	for rest != "" {
		keyword, after := splitFirstOrJSON(rest)
		keyword = strings.ToLower(keyword)

		switch keyword {
		case "where":
			jsonStr, remaining, err := extractJSON(after)
			if err != nil {
				return nil, parseErr("where requires a JSON object", `where {"meal_date": "2026-04-17"}`)
			}
			filters, err := parseWhereJSON(jsonStr)
			if err != nil {
				return nil, err
			}
			cmd.Where = filters
			rest = remaining

		case "sort":
			field, remaining := splitFirst(after)
			if field == "" {
				return nil, parseErr("sort requires a field name", "sort created_at desc")
			}
			order := "asc"
			next, remaining2 := splitFirst(remaining)
			if strings.ToLower(next) == "desc" || strings.ToLower(next) == "asc" {
				order = strings.ToLower(next)
				rest = remaining2
			} else {
				rest = remaining
			}
			cmd.Sort = append(cmd.Sort, DBSort{Field: field, Order: order})

		case "limit":
			numStr, remaining := splitFirst(after)
			n := parseInt(numStr)
			if n <= 0 {
				return nil, parseErr("limit requires a positive number", "limit 20")
			}
			if n > maxLimit {
				n = maxLimit
			}
			cmd.Limit = int(n)
			rest = remaining

		case "group_by":
			jsonStr, remaining, err := extractJSON(after)
			if err != nil {
				return nil, parseErr("group_by requires a JSON array", `group_by ["meal_date", "meal_type"]`)
			}
			var fields []string
			if err := json.Unmarshal([]byte(jsonStr), &fields); err != nil {
				return nil, parseErr(fmt.Sprintf("invalid group_by: %v", err), `group_by ["meal_date"]`)
			}
			cmd.GroupBy = fields
			rest = remaining

		case "sum", "avg", "min", "max":
			jsonStr, remaining, err := extractJSON(after)
			if err != nil {
				return nil, parseErr(fmt.Sprintf("%s requires a JSON array", keyword), fmt.Sprintf(`%s ["calories"]`, keyword))
			}
			var fields []string
			if err := json.Unmarshal([]byte(jsonStr), &fields); err != nil {
				return nil, parseErr(fmt.Sprintf("invalid %s: %v", keyword, err), fmt.Sprintf(`%s ["calories"]`, keyword))
			}
			cmd.Aggregates[keyword] = fields
			rest = remaining

		default:
			return nil, parseErr(fmt.Sprintf("unexpected keyword %q", keyword), "valid clauses: where, sort, limit, group_by, sum, avg, min, max")
		}
	}
	return cmd, nil
}

func parseWhereJSON(jsonStr string) ([]DBFilter, error) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return nil, parseErr(fmt.Sprintf("invalid where: %v", err), `where {"meal_date": "2026-04-17"}`)
	}
	var filters []DBFilter
	for key, val := range raw {
		field, op := parseFieldOp(key)
		filters = append(filters, DBFilter{Field: field, Op: op, Value: val})
	}
	// Stable order for normalized output.
	sort.Slice(filters, func(i, j int) bool { return filters[i].Field < filters[j].Field })
	return filters, nil
}

func parseFieldOp(key string) (string, string) {
	ops := []string{"__gte", "__gt", "__lte", "__lt", "__contains", "__neq"}
	for _, suffix := range ops {
		if strings.HasSuffix(key, suffix) {
			return key[:len(key)-len(suffix)], suffix[2:]
		}
	}
	return key, "eq"
}

// --- Executor ---

type DBExecutor struct {
	db *sql.DB
}

func NewDBExecutor(db *sql.DB) *DBExecutor {
	return &DBExecutor{db: db}
}

func (e *DBExecutor) Execute(ctx context.Context, cmd *DBCommand) (string, error) {
	switch cmd.Verb {
	case "list":
		return e.execList(ctx)
	case "describe":
		return e.execDescribe(ctx, cmd.Table)
	case "create":
		return e.execCreate(ctx, cmd)
	case "query":
		return e.execQuery(ctx, cmd)
	case "count":
		return e.execCount(ctx, cmd)
	case "insert":
		return e.execInsert(ctx, cmd)
	case "update":
		return e.execUpdate(ctx, cmd)
	default:
		return "", fmt.Errorf("unknown verb: %s", cmd.Verb)
	}
}

func (e *DBExecutor) execList(ctx context.Context) (string, error) {
	rows, err := e.db.QueryContext(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name LIKE 'argus_%'
		ORDER BY table_name
	`)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		tables = append(tables, strings.TrimPrefix(name, dbPrefix))
	}
	if len(tables) == 0 {
		return "No tables found.", nil
	}
	return "Tables: " + strings.Join(tables, ", "), nil
}

func (e *DBExecutor) execDescribe(ctx context.Context, table string) (string, error) {
	rows, err := e.db.QueryContext(ctx, `
		SELECT column_name, data_type, is_nullable
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1
		ORDER BY ordinal_position
	`, dbPrefix+table)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type col struct{ Name, Type, Nullable string }
	var cols []col
	for rows.Next() {
		var c col
		rows.Scan(&c.Name, &c.Type, &c.Nullable)
		cols = append(cols, c)
	}
	if len(cols) == 0 {
		return "", execErr(fmt.Sprintf("table %q does not exist", table), "list")
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Table: %s\n", table))
	for _, c := range cols {
		req := ""
		if c.Nullable == "NO" {
			req = " (required)"
		}
		sb.WriteString(fmt.Sprintf("  %s: %s%s\n", c.Name, pgTypeToDSL(c.Type), req))
	}
	return sb.String(), nil
}

func (e *DBExecutor) execCreate(ctx context.Context, cmd *DBCommand) (string, error) {
	pgTable := quoteIdent(dbPrefix + cmd.Table)

	// Build column definitions.
	var colDefs []string
	colDefs = append(colDefs, "id BIGSERIAL PRIMARY KEY")

	// Sort column names for deterministic DDL.
	names := make([]string, 0, len(cmd.Columns))
	for name := range cmd.Columns {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		typ := cmd.Columns[name]
		required := strings.HasSuffix(typ, "!")
		base := strings.TrimSuffix(typ, "!")
		pgType := dslTypeToPG(base)
		notNull := ""
		if required {
			notNull = " NOT NULL"
		}
		colDefs = append(colDefs, fmt.Sprintf("%s %s%s", quoteIdent(name), pgType, notNull))
	}

	colDefs = append(colDefs,
		"created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()",
		"updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()",
	)

	ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n  %s\n)", pgTable, strings.Join(colDefs, ",\n  "))
	if _, err := e.db.ExecContext(ctx, ddl); err != nil {
		return "", fmt.Errorf("create table: %w", err)
	}

	// Return current schema (handles both new and existing tables).
	return e.execDescribe(ctx, cmd.Table)
}

func (e *DBExecutor) execQuery(ctx context.Context, cmd *DBCommand) (string, error) {
	pgTable := quoteIdent(dbPrefix + cmd.Table)
	limit := cmd.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	// Build query.
	args := []any{}
	where, args := buildWhere(cmd.Where, args)

	orderBy := "ORDER BY created_at DESC"
	if len(cmd.Sort) > 0 {
		var parts []string
		for _, s := range cmd.Sort {
			parts = append(parts, fmt.Sprintf("%s %s", quoteIdent(s.Field), strings.ToUpper(s.Order)))
		}
		orderBy = "ORDER BY " + strings.Join(parts, ", ")
	}

	query := fmt.Sprintf("SELECT * FROM %s%s %s LIMIT %d", pgTable, where, orderBy, limit)

	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return "", fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	return rowsToJSON(rows)
}

func (e *DBExecutor) execCount(ctx context.Context, cmd *DBCommand) (string, error) {
	pgTable := quoteIdent(dbPrefix + cmd.Table)
	args := []any{}
	where, args := buildWhere(cmd.Where, args)

	if len(cmd.GroupBy) == 0 && len(cmd.Aggregates) == 0 {
		// Simple count.
		var count int64
		query := fmt.Sprintf("SELECT COUNT(*) FROM %s%s", pgTable, where)
		if err := e.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
			return "", fmt.Errorf("count: %w", err)
		}
		return fmt.Sprintf(`{"count": %d}`, count), nil
	}

	// Grouped aggregation.
	var selectParts []string
	var quotedGroupBy []string
	for _, g := range cmd.GroupBy {
		selectParts = append(selectParts, quoteIdent(g))
		quotedGroupBy = append(quotedGroupBy, quoteIdent(g))
	}
	selectParts = append(selectParts, "COUNT(*) AS _count")
	for fn, fields := range cmd.Aggregates {
		for _, f := range fields {
			selectParts = append(selectParts, fmt.Sprintf("%s(%s) AS %s_%s", strings.ToUpper(fn), quoteIdent(f), fn, f))
		}
	}

	groupBy := ""
	if len(quotedGroupBy) > 0 {
		groupBy = " GROUP BY " + strings.Join(quotedGroupBy, ", ")
	}

	query := fmt.Sprintf("SELECT %s FROM %s%s%s ORDER BY 1",
		strings.Join(selectParts, ", "), pgTable, where, groupBy)

	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return "", fmt.Errorf("count: %w", err)
	}
	defer rows.Close()

	return rowsToJSON(rows)
}

func (e *DBExecutor) execInsert(ctx context.Context, cmd *DBCommand) (string, error) {
	pgTable := quoteIdent(dbPrefix + cmd.Table)
	rows := cmd.Rows
	if cmd.Data != nil {
		rows = []map[string]any{cmd.Data}
	}
	if len(rows) == 0 {
		return "", execErr("no data to insert", `insert food_log {"name": "test"}`)
	}

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}

	var ids []int64
	for _, row := range rows {
		cols := sortedKeys(row)
		quotedCols := make([]string, len(cols))
		placeholders := make([]string, len(cols))
		vals := make([]any, len(cols))
		for i, c := range cols {
			quotedCols[i] = quoteIdent(c)
			placeholders[i] = fmt.Sprintf("$%d", i+1)
			vals[i] = row[c]
		}

		query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) RETURNING id",
			pgTable, strings.Join(quotedCols, ", "), strings.Join(placeholders, ", "))

		var id int64
		if err := tx.QueryRowContext(ctx, query, vals...).Scan(&id); err != nil {
			tx.Rollback()
			return "", fmt.Errorf("insert: %w", err)
		}
		ids = append(ids, id)
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}

	data, _ := json.Marshal(map[string]any{"inserted": len(ids), "ids": ids})
	return string(data), nil
}

func (e *DBExecutor) execUpdate(ctx context.Context, cmd *DBCommand) (string, error) {
	pgTable := quoteIdent(dbPrefix + cmd.Table)
	if cmd.ID <= 0 {
		return "", execErr("update requires a valid id", "update food_log 42 {...}")
	}

	cols := sortedKeys(cmd.Data)
	setParts := make([]string, len(cols)+1)
	args := make([]any, len(cols)+1)
	for i, c := range cols {
		setParts[i] = fmt.Sprintf("%s = $%d", quoteIdent(c), i+1)
		args[i] = cmd.Data[c]
	}
	setParts[len(cols)] = fmt.Sprintf("updated_at = $%d", len(cols)+1)
	args[len(cols)] = time.Now()

	query := fmt.Sprintf("UPDATE %s SET %s WHERE id = $%d",
		pgTable, strings.Join(setParts, ", "), len(args)+1)
	args = append(args, cmd.ID)

	res, err := e.db.ExecContext(ctx, query, args...)
	if err != nil {
		return "", fmt.Errorf("update: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return "", execErr(fmt.Sprintf("no row with id=%d in table %q", cmd.ID, cmd.Table), "query "+cmd.Table+" limit 5")
	}
	return fmt.Sprintf(`{"updated": %d, "id": %d}`, n, cmd.ID), nil
}

// --- Validator ---

// ValidateColumns checks that all referenced columns exist in the table.
// Returns a helpful error with fuzzy suggestions if a column is not found.
func (e *DBExecutor) ValidateColumns(ctx context.Context, table string, fieldNames []string) error {
	if len(fieldNames) == 0 {
		return nil
	}
	rows, err := e.db.QueryContext(ctx, `
		SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1
	`, dbPrefix+table)
	if err != nil {
		return err
	}
	defer rows.Close()

	validCols := map[string]bool{}
	var colList []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		validCols[name] = true
		colList = append(colList, name)
	}
	if len(colList) == 0 {
		return fmt.Errorf("table %q does not exist. Use: list", table)
	}

	for _, f := range fieldNames {
		if !validCols[f] {
			suggestion := fuzzyMatch(f, colList)
			hint := fmt.Sprintf("Available columns: %s", strings.Join(colList, ", "))
			if suggestion != "" {
				hint = fmt.Sprintf("Did you mean %q? %s", suggestion, hint)
			}
			return fmt.Errorf("column %q does not exist in table %q.\n%s", f, table, hint)
		}
	}
	return nil
}

// --- Helpers ---

func buildWhere(filters []DBFilter, args []any) (string, []any) {
	if len(filters) == 0 {
		return "", args
	}
	var parts []string
	for _, f := range filters {
		idx := len(args) + 1
		qf := quoteIdent(f.Field)
		switch f.Op {
		case "eq":
			parts = append(parts, fmt.Sprintf("%s = $%d", qf, idx))
		case "gt":
			parts = append(parts, fmt.Sprintf("%s > $%d", qf, idx))
		case "gte":
			parts = append(parts, fmt.Sprintf("%s >= $%d", qf, idx))
		case "lt":
			parts = append(parts, fmt.Sprintf("%s < $%d", qf, idx))
		case "lte":
			parts = append(parts, fmt.Sprintf("%s <= $%d", qf, idx))
		case "neq":
			parts = append(parts, fmt.Sprintf("%s != $%d", qf, idx))
		case "contains":
			parts = append(parts, fmt.Sprintf("%s ILIKE $%d", qf, idx))
			f.Value = fmt.Sprintf("%%%v%%", f.Value)
		default:
			parts = append(parts, fmt.Sprintf("%s = $%d", qf, idx))
		}
		args = append(args, f.Value)
	}
	return " WHERE " + strings.Join(parts, " AND "), args
}

func rowsToJSON(rows *sql.Rows) (string, error) {
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	colTypes, _ := rows.ColumnTypes()

	var results []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = formatValue(vals[i], colTypes[i])
		}
		results = append(results, row)
	}

	if len(results) == 0 {
		return "[]", nil
	}
	data, _ := json.Marshal(results)
	return string(data), nil
}

func formatValue(v any, ct *sql.ColumnType) any {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case []byte:
		return string(val)
	case time.Time:
		if ct != nil && ct.DatabaseTypeName() == "DATE" {
			return val.Format("2006-01-02")
		}
		return val.Format(time.RFC3339)
	case float64:
		if val == math.Trunc(val) {
			return int64(val)
		}
		return val
	default:
		return val
	}
}

func dslTypeToPG(t string) string {
	switch t {
	case "text":
		return "TEXT"
	case "number":
		return "DOUBLE PRECISION"
	case "date":
		return "DATE"
	case "boolean":
		return "BOOLEAN"
	case "timestamp":
		return "TIMESTAMPTZ"
	case "json":
		return "JSONB"
	default:
		return "TEXT"
	}
}

func pgTypeToDSL(t string) string {
	switch strings.ToLower(t) {
	case "text", "character varying":
		return "text"
	case "double precision", "numeric", "integer", "bigint", "real":
		return "number"
	case "date":
		return "date"
	case "boolean":
		return "boolean"
	case "timestamp with time zone", "timestamp without time zone":
		return "timestamp"
	case "jsonb", "json":
		return "json"
	default:
		return t
	}
}

func isValidType(t string) bool {
	switch t {
	case "text", "number", "date", "boolean", "timestamp", "json":
		return true
	}
	return false
}

// identifierRe restricts table/column names to a safe subset: lowercase
// alpha start, then alphanumeric + underscore, max 63 chars (PG limit).
var identifierRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// validateIdentifier checks that a name is a safe SQL identifier.
func validateIdentifier(name, kind string) error {
	if !identifierRe.MatchString(name) {
		return fmt.Errorf("Error: invalid %s name %q — must match [a-z][a-z0-9_]{0,62}\nExample: food_log", kind, name)
	}
	return nil
}

// quoteIdent wraps a validated identifier in double quotes for safe SQL splicing.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func splitFirst(s string) (string, string) {
	s = strings.TrimSpace(s)
	idx := strings.IndexByte(s, ' ')
	if idx < 0 {
		return s, ""
	}
	return s[:idx], strings.TrimSpace(s[idx+1:])
}

func splitFirstOrJSON(s string) (string, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if s[0] == '{' || s[0] == '[' {
		return string(s[0]), s // JSON starts here, return the bracket as keyword
	}
	return splitFirst(s)
}

func extractJSON(s string) (string, string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", fmt.Errorf("expected JSON")
	}
	open := s[0]
	var close byte
	switch open {
	case '{':
		close = '}'
	case '[':
		close = ']'
	default:
		return "", "", fmt.Errorf("expected { or [, got %c", open)
	}

	depth := 0
	inStr := false
	escaped := false
	for i := 0; i < len(s); i++ {
		if escaped {
			escaped = false
			continue
		}
		c := s[i]
		if c == '\\' && inStr {
			escaped = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		if c == open {
			depth++
		} else if c == close {
			depth--
			if depth == 0 {
				return s[:i+1], strings.TrimSpace(s[i+1:]), nil
			}
		}
	}
	return "", "", fmt.Errorf("unclosed JSON")
}

func parseInt(s string) int64 {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

func parseErr(msg, example string) error {
	return fmt.Errorf("Error: %s\nExample: %s", msg, example)
}

func execErr(msg, example string) error {
	return fmt.Errorf("Error: %s\nExample: %s", msg, example)
}

// fuzzyMatch finds the closest column name by Levenshtein distance.
func fuzzyMatch(input string, candidates []string) string {
	best := ""
	bestDist := 999
	for _, c := range candidates {
		d := levenshtein(strings.ToLower(input), strings.ToLower(c))
		if d < bestDist && d <= 3 { // only suggest if reasonably close
			bestDist = d
			best = c
		}
	}
	return best
}

func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	d := make([][]int, la+1)
	for i := range d {
		d[i] = make([]int, lb+1)
		d[i][0] = i
	}
	for j := 0; j <= lb; j++ {
		d[0][j] = j
	}
	for i := 1; i <= la; i++ {
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[i][j] = min3(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
		}
	}
	return d[la][lb]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

// pq is needed for array scanning in some queries.
var _ = pq.Array
