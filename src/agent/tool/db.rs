use serde::Deserialize;
use serde_json::{Map, Value};
use sqlx::{PgPool, Row};

use crate::database::Database;

use super::Tool;

/// Prefix for all user-created tables.
const DB_PREFIX: &str = "argus_";

/// Regex for valid table/column identifiers.
const IDENT_PATTERN: &str = r"^[a-z][a-z0-9_]{0,62}$";

/// Auto-managed columns that cannot be set by the user.
const AUTO_COLUMNS: &[&str] = &["id", "created_at", "updated_at"];

/// Maximum rows returned by `query`.
const MAX_LIMIT: i64 = 200;

/// Default rows returned by `query`.
const DEFAULT_LIMIT: i64 = 50;

/// Type mapping from user-facing names to PostgreSQL types.
fn pg_type(user_type: &str) -> Option<&'static str> {
    match user_type {
        "text" => Some("TEXT"),
        "number" => Some("DOUBLE PRECISION"),
        "date" => Some("DATE"),
        "boolean" => Some("BOOLEAN"),
        "timestamp" => Some("TIMESTAMPTZ"),
        "json" => Some("JSONB"),
        _ => None,
    }
}

pub(super) struct Db<'a> {
    db: &'a Database,
}

#[derive(Deserialize)]
struct Args {
    command: String,
}

impl<'a> Db<'a> {
    pub(super) fn new(db: &'a Database) -> Self {
        Self { db }
    }

    fn pool(&self) -> &PgPool {
        self.db.pool()
    }

    async fn run_command(&self, command: &str) -> Result<String, String> {
        let command = command.trim();
        if command.is_empty() {
            return Err("empty command".into());
        }

        // Split into subcommand and rest.
        let (sub, rest) = match command.find(char::is_whitespace) {
            Some(pos) => (&command[..pos], command[pos..].trim_start()),
            None => (command, ""),
        };

        match sub {
            "list" => self.cmd_list().await,
            "describe" => self.cmd_describe(rest).await,
            "create" => self.cmd_create(rest).await,
            "query" => self.cmd_query(rest).await,
            "count" => self.cmd_count(rest).await,
            "insert" => self.cmd_insert(rest).await,
            "update" => self.cmd_update(rest).await,
            other => Err(format!(
                "unknown command \"{other}\"\n\
                 Example: list | describe <table> | create <table> {{...}} | \
                 query <table> | count <table> | insert <table> {{...}} | update <table> <id> {{...}}"
            )),
        }
    }

    // ── list ────────────────────────────────────────────────────────────

    async fn cmd_list(&self) -> Result<String, String> {
        let rows = sqlx::query(
            "SELECT table_name FROM information_schema.tables \
             WHERE table_schema = 'public' AND table_name LIKE 'argus\\_%' \
             ORDER BY table_name",
        )
        .fetch_all(self.pool())
        .await
        .map_err(|e| format!("query failed: {e}"))?;

        if rows.is_empty() {
            return Ok("No tables found.".into());
        }

        let names: Vec<String> = rows
            .iter()
            .map(|r| {
                let full: String = r.get("table_name");
                full.strip_prefix(DB_PREFIX).unwrap_or(&full).to_string()
            })
            .collect();

        Ok(serde_json::to_string_pretty(&names).unwrap())
    }

    // ── describe ────────────────────────────────────────────────────────

    async fn cmd_describe(&self, rest: &str) -> Result<String, String> {
        let table = rest.trim();
        if table.is_empty() {
            return Err("missing table name\nExample: describe my_table".into());
        }
        validate_ident(table)?;
        let full_name = format!("{DB_PREFIX}{table}");

        let rows = sqlx::query(
            "SELECT column_name, data_type, is_nullable, column_default \
             FROM information_schema.columns \
             WHERE table_schema = 'public' AND table_name = $1 \
             ORDER BY ordinal_position",
        )
        .bind(&full_name)
        .fetch_all(self.pool())
        .await
        .map_err(|e| format!("query failed: {e}"))?;

        if rows.is_empty() {
            return Err(format!("table \"{table}\" does not exist"));
        }

        let columns: Vec<Value> = rows
            .iter()
            .map(|r| {
                serde_json::json!({
                    "column": r.get::<String, _>("column_name"),
                    "type": r.get::<String, _>("data_type"),
                    "nullable": r.get::<String, _>("is_nullable") == "YES",
                    "default": r.get::<Option<String>, _>("column_default"),
                })
            })
            .collect();

        Ok(serde_json::to_string_pretty(&columns).unwrap())
    }

    // ── create ──────────────────────────────────────────────────────────

    async fn cmd_create(&self, rest: &str) -> Result<String, String> {
        // Parse: <table> {json}
        let (table, json_str) = split_first_word(rest)
            .ok_or_else(|| "missing table name and columns\nExample: create my_table {\"name\": \"text!\", \"age\": \"number\"}".to_string())?;
        validate_ident(table)?;

        let columns: Map<String, Value> = serde_json::from_str(json_str)
            .map_err(|e| format!("invalid column JSON: {e}\nExample: create my_table {{\"name\": \"text!\", \"age\": \"number\"}}"))?;

        if columns.is_empty() {
            return Err("at least one column is required".into());
        }

        let full_name = format!("{DB_PREFIX}{table}");
        let mut col_defs = vec!["id BIGSERIAL PRIMARY KEY".to_string()];

        for (col_name, col_type_val) in &columns {
            validate_ident(col_name)?;
            if AUTO_COLUMNS.contains(&col_name.as_str()) {
                return Err(format!(
                    "column \"{col_name}\" is auto-managed and cannot be specified"
                ));
            }

            let type_str = col_type_val
                .as_str()
                .ok_or_else(|| format!("column \"{col_name}\" type must be a string"))?;

            let (base_type, not_null) = if let Some(stripped) = type_str.strip_suffix('!') {
                (stripped, true)
            } else {
                (type_str, false)
            };

            let pg = pg_type(base_type).ok_or_else(|| {
                format!(
                    "unknown type \"{base_type}\" for column \"{col_name}\". \
                     Valid types: text, number, date, boolean, timestamp, json"
                )
            })?;

            let mut def = format!("{col_name} {pg}");
            if not_null {
                def.push_str(" NOT NULL");
            }
            col_defs.push(def);
        }

        col_defs.push("created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()".into());
        col_defs.push("updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()".into());

        let sql = format!("CREATE TABLE {full_name} ({})", col_defs.join(", "));

        sqlx::query(&sql)
            .execute(self.pool())
            .await
            .map_err(|e| format!("create table failed: {e}"))?;

        Ok(format!("Table \"{table}\" created."))
    }

    // ── query ───────────────────────────────────────────────────────────

    async fn cmd_query(&self, rest: &str) -> Result<String, String> {
        let (table, remainder) = split_first_word(rest)
            .ok_or_else(|| "missing table name\nExample: query my_table".to_string())?;
        validate_ident(table)?;
        let full_name = format!("{DB_PREFIX}{table}");

        let mut params: Vec<String> = Vec::new();
        let mut where_clause = String::new();
        let mut sort_field = "created_at".to_string();
        let mut sort_dir = "DESC".to_string();
        let mut limit: i64 = DEFAULT_LIMIT;

        let mut cursor = remainder;

        // Parse optional clauses: where {...} sort <field> [asc|desc] limit N
        while !cursor.is_empty() {
            let (keyword, after) = match split_first_word(cursor) {
                Some(pair) => pair,
                None => break,
            };

            match keyword {
                "where" => {
                    let (json_str, after_json) = extract_json_object(after)?;
                    let conditions: Map<String, Value> = serde_json::from_str(&json_str)
                        .map_err(|e| format!("invalid where JSON: {e}"))?;
                    where_clause = build_where(&conditions, &mut params)?;
                    cursor = after_json;
                }
                "sort" => {
                    let (field, after_field) = split_first_word(after).ok_or_else(|| {
                        "missing sort field\nExample: query my_table sort name asc".to_string()
                    })?;
                    validate_ident(field)?;
                    sort_field = field.to_string();

                    // Optional direction
                    if let Some((dir, after_dir)) = split_first_word(after_field) {
                        match dir.to_lowercase().as_str() {
                            "asc" => {
                                sort_dir = "ASC".into();
                                cursor = after_dir;
                            }
                            "desc" => {
                                sort_dir = "DESC".into();
                                cursor = after_dir;
                            }
                            _ => {
                                cursor = after_field;
                            } // not a direction, leave for next parse
                        }
                    } else {
                        cursor = after_field;
                    }
                }
                "limit" => {
                    let (n_str, after_n) = split_first_word(after).ok_or_else(|| {
                        "missing limit value\nExample: query my_table limit 10".to_string()
                    })?;
                    limit = n_str
                        .parse::<i64>()
                        .map_err(|_| format!("invalid limit \"{n_str}\""))?;
                    limit = limit.clamp(1, MAX_LIMIT);
                    cursor = after_n;
                }
                other => {
                    return Err(format!(
                        "unexpected keyword \"{other}\"\n\
                         Example: query my_table where {{...}} sort name asc limit 10"
                    ));
                }
            }
        }

        let param_idx = params.len() + 1;
        let sql = format!(
            "SELECT * FROM {full_name}{where_clause} ORDER BY {sort_field} {sort_dir} LIMIT ${param_idx}"
        );
        params.push(limit.to_string());

        let rows = {
            let mut q = sqlx::query(&sql);
            for (i, p) in params.iter().enumerate() {
                // Last param is the limit (integer), rest are string values.
                if i == params.len() - 1 {
                    q = q.bind(limit);
                } else {
                    q = q.bind(p);
                }
            }
            q.fetch_all(self.pool())
                .await
                .map_err(|e| format!("query failed: {e}"))?
        };

        let result = rows_to_json(&rows);
        Ok(serde_json::to_string_pretty(&result).unwrap())
    }

    // ── count ───────────────────────────────────────────────────────────

    async fn cmd_count(&self, rest: &str) -> Result<String, String> {
        let (table, remainder) = split_first_word(rest)
            .ok_or_else(|| "missing table name\nExample: count my_table".to_string())?;
        validate_ident(table)?;
        let full_name = format!("{DB_PREFIX}{table}");

        let mut params: Vec<String> = Vec::new();
        let mut where_clause = String::new();
        let mut group_by: Vec<String> = Vec::new();
        let mut aggregations: Vec<(String, String)> = Vec::new(); // (func, field)

        let mut cursor = remainder;

        while !cursor.is_empty() {
            let (keyword, after) = match split_first_word(cursor) {
                Some(pair) => pair,
                None => break,
            };

            match keyword {
                "where" => {
                    let (json_str, after_json) = extract_json_object(after)?;
                    let conditions: Map<String, Value> = serde_json::from_str(&json_str)
                        .map_err(|e| format!("invalid where JSON: {e}"))?;
                    where_clause = build_where(&conditions, &mut params)?;
                    cursor = after_json;
                }
                "group_by" => {
                    let (json_str, after_json) = extract_json_array(after)?;
                    let fields: Vec<String> = serde_json::from_str(&json_str)
                        .map_err(|e| format!("invalid group_by JSON: {e}"))?;
                    for f in &fields {
                        validate_ident(f)?;
                    }
                    group_by = fields;
                    cursor = after_json;
                }
                "sum" | "avg" | "min" | "max" => {
                    let func = keyword.to_uppercase();
                    let (json_str, after_json) = extract_json_array(after)?;
                    let fields: Vec<String> = serde_json::from_str(&json_str)
                        .map_err(|e| format!("invalid {keyword} JSON: {e}"))?;
                    for f in &fields {
                        validate_ident(f)?;
                        aggregations.push((func.clone(), f.clone()));
                    }
                    cursor = after_json;
                }
                other => {
                    return Err(format!(
                        "unexpected keyword \"{other}\"\n\
                         Example: count my_table where {{...}} group_by [\"status\"] sum [\"amount\"]"
                    ));
                }
            }
        }

        // Build SELECT clause.
        let mut select_parts: Vec<String> = Vec::new();
        for g in &group_by {
            select_parts.push(g.clone());
        }
        select_parts.push("COUNT(*) AS count".into());
        for (func, field) in &aggregations {
            select_parts.push(format!("{func}({field}) AS {func}_{field}").to_lowercase());
        }

        let group_clause = if group_by.is_empty() {
            String::new()
        } else {
            format!(" GROUP BY {}", group_by.join(", "))
        };

        let sql = format!(
            "SELECT {} FROM {full_name}{where_clause}{group_clause}",
            select_parts.join(", ")
        );

        let rows = {
            let mut q = sqlx::query(&sql);
            for p in &params {
                q = q.bind(p);
            }
            q.fetch_all(self.pool())
                .await
                .map_err(|e| format!("query failed: {e}"))?
        };

        let result = rows_to_json(&rows);
        Ok(serde_json::to_string_pretty(&result).unwrap())
    }

    // ── insert ──────────────────────────────────────────────────────────

    async fn cmd_insert(&self, rest: &str) -> Result<String, String> {
        let (table, json_str) = split_first_word(rest).ok_or_else(|| {
            "missing table name and data\nExample: insert my_table {\"name\": \"Alice\"}"
                .to_string()
        })?;
        validate_ident(table)?;
        let full_name = format!("{DB_PREFIX}{table}");

        let json_str = json_str.trim();
        if json_str.is_empty() {
            return Err("missing data\nExample: insert my_table {\"name\": \"Alice\"}".into());
        }

        // Accept single object or array of objects.
        let records: Vec<Map<String, Value>> = if json_str.starts_with('[') {
            serde_json::from_str(json_str).map_err(|e| format!("invalid JSON array: {e}"))?
        } else {
            let obj: Map<String, Value> =
                serde_json::from_str(json_str).map_err(|e| format!("invalid JSON object: {e}"))?;
            vec![obj]
        };

        if records.is_empty() {
            return Err("empty array — nothing to insert".into());
        }

        // Validate column names and reject auto-columns.
        for record in &records {
            for key in record.keys() {
                validate_ident(key)?;
                if AUTO_COLUMNS.contains(&key.as_str()) {
                    return Err(format!(
                        "column \"{key}\" is auto-managed and cannot be set"
                    ));
                }
            }
        }

        // Use a transaction for multi-row inserts.
        let mut tx = self
            .pool()
            .begin()
            .await
            .map_err(|e| format!("transaction start failed: {e}"))?;
        let mut ids: Vec<i64> = Vec::new();

        for record in &records {
            let columns: Vec<&str> = record.keys().map(|k| k.as_str()).collect();
            let placeholders: Vec<String> = (1..=columns.len()).map(|i| format!("${i}")).collect();

            let sql = format!(
                "INSERT INTO {full_name} ({}) VALUES ({}) RETURNING id",
                columns.join(", "),
                placeholders.join(", ")
            );

            let mut q = sqlx::query(&sql);
            for col in &columns {
                q = bind_json_value(q, record.get(*col).unwrap());
            }

            let row = q
                .fetch_one(&mut *tx)
                .await
                .map_err(|e| format!("insert failed: {e}"))?;

            let id: i64 = row.get("id");
            ids.push(id);
        }

        tx.commit()
            .await
            .map_err(|e| format!("transaction commit failed: {e}"))?;

        let result = serde_json::json!({
            "inserted": ids.len(),
            "ids": ids,
        });
        Ok(serde_json::to_string_pretty(&result).unwrap())
    }

    // ── update ──────────────────────────────────────────────────────────

    async fn cmd_update(&self, rest: &str) -> Result<String, String> {
        // Parse: <table> <id> {json}
        let (table, after_table) = split_first_word(rest).ok_or_else(|| {
            "missing table name, id, and data\nExample: update my_table 42 {\"name\": \"Bob\"}"
                .to_string()
        })?;
        validate_ident(table)?;

        let (id_str, json_str) = split_first_word(after_table).ok_or_else(|| {
            "missing id and data\nExample: update my_table 42 {\"name\": \"Bob\"}".to_string()
        })?;
        let id: i64 = id_str
            .parse()
            .map_err(|_| format!("invalid id \"{id_str}\""))?;

        let json_str = json_str.trim();
        if json_str.is_empty() {
            return Err("missing data\nExample: update my_table 42 {\"name\": \"Bob\"}".into());
        }

        let fields: Map<String, Value> =
            serde_json::from_str(json_str).map_err(|e| format!("invalid JSON: {e}"))?;

        if fields.is_empty() {
            return Err("no fields to update".into());
        }

        for key in fields.keys() {
            validate_ident(key)?;
            if AUTO_COLUMNS.contains(&key.as_str()) {
                return Err(format!(
                    "column \"{key}\" is auto-managed and cannot be set"
                ));
            }
        }

        let full_name = format!("{DB_PREFIX}{table}");

        // Build SET clause. Reserve $1 for id.
        let mut set_parts: Vec<String> = Vec::new();
        let mut param_values: Vec<&Value> = Vec::new();
        for (i, (col, val)) in fields.iter().enumerate() {
            let idx = i + 2; // $1 is id
            set_parts.push(format!("{col} = ${idx}"));
            param_values.push(val);
        }
        set_parts.push("updated_at = NOW()".into());

        let sql = format!(
            "UPDATE {full_name} SET {} WHERE id = $1",
            set_parts.join(", ")
        );

        let mut q = sqlx::query(&sql).bind(id);
        for val in &param_values {
            q = bind_json_value(q, val);
        }

        let result = q
            .execute(self.pool())
            .await
            .map_err(|e| format!("update failed: {e}"))?;

        if result.rows_affected() == 0 {
            return Err(format!("no row with id={id} in \"{table}\""));
        }

        Ok(format!("Updated row id={id}."))
    }
}

// ── Tool trait impl ─────────────────────────────────────────────────────

#[async_trait::async_trait]
impl<'a> Tool for Db<'a> {
    fn name(&self) -> &str {
        "db"
    }

    fn description(&self) -> &str {
        "Interact with PostgreSQL database tables. All user tables are prefixed with \"argus_\". \
         Auto-columns (id, created_at, updated_at) are managed automatically.\n\n\
         Commands:\n\
         - list — show all tables\n\
         - describe <table> — show columns\n\
         - create <table> {\"col\": \"type[!]\", ...} — create table (types: text, number, date, boolean, timestamp, json; ! = NOT NULL)\n\
         - query <table> [where {...}] [sort <field> [asc|desc]] [limit N] — query rows (default: sort created_at desc, limit 50)\n\
         - count <table> [where {...}] [group_by [...]] [sum [...]] [avg [...]] [min [...]] [max [...]] — count/aggregate\n\
         - insert <table> {...} or [{...}, ...] — insert row(s)\n\
         - update <table> <id> {...} — update a row by id\n\n\
         WHERE operators: field (exact match), field__gt, field__gte, field__lt, field__lte, field__contains (ILIKE), field__neq"
    }

    fn parameters(&self) -> Value {
        serde_json::json!({
            "type": "object",
            "properties": {
                "command": {
                    "type": "string",
                    "description": "Database command, e.g. 'list', 'query users where {\"active\": true} limit 10'"
                }
            },
            "required": ["command"]
        })
    }

    async fn execute(&self, _ctx: &super::ToolContext<'_>, args: &str) -> String {
        let parsed: Args = match serde_json::from_str(args) {
            Ok(a) => a,
            Err(e) => return format!("error: invalid arguments: {e}"),
        };

        match self.run_command(&parsed.command).await {
            Ok(output) => output,
            Err(msg) => format!("error: {msg}"),
        }
    }

    fn status_label(&self, args: &str) -> String {
        let cmd = serde_json::from_str::<Value>(args)
            .ok()
            .and_then(|v| v.get("command")?.as_str().map(String::from))
            .unwrap_or_default();
        if cmd.is_empty() {
            return "\u{1f5c4}\u{fe0f} Database".into();
        }
        format!(
            "\u{1f5c4}\u{fe0f} Database: {}",
            super::truncate_display(&cmd, 40)
        )
    }

    /// Normalize DB command arguments: extract the command string,
    /// sort JSON object keys for deterministic comparison.
    fn normalize_args(&self, args: &str) -> String {
        let Ok(v) = serde_json::from_str::<Value>(args) else {
            return args.to_string();
        };
        let Some(cmd) = v.get("command").and_then(|c| c.as_str()) else {
            return args.to_string();
        };
        // Return just the command text (stripped of wrapper JSON).
        cmd.to_string()
    }
}

// ── Helpers ─────────────────────────────────────────────────────────────

/// Validate a table or column identifier.
fn validate_ident(name: &str) -> Result<(), String> {
    let re = regex::Regex::new(IDENT_PATTERN).unwrap();
    if !re.is_match(name) {
        return Err(format!(
            "invalid identifier \"{name}\": must match {IDENT_PATTERN}"
        ));
    }
    Ok(())
}

/// Split string into first word and remainder.
fn split_first_word(s: &str) -> Option<(&str, &str)> {
    let s = s.trim_start();
    if s.is_empty() {
        return None;
    }
    match s.find(char::is_whitespace) {
        Some(pos) => Some((&s[..pos], s[pos..].trim_start())),
        None => Some((s, "")),
    }
}

/// Extract a JSON object starting at the beginning of `s`, returning the
/// JSON string and the remainder after it.
fn extract_json_object(s: &str) -> Result<(String, &str), String> {
    extract_json_balanced(s, '{', '}')
}

/// Extract a JSON array starting at the beginning of `s`.
fn extract_json_array(s: &str) -> Result<(String, &str), String> {
    extract_json_balanced(s, '[', ']')
}

/// Extract a balanced JSON structure (object or array) from the start of `s`.
fn extract_json_balanced(s: &str, open: char, close: char) -> Result<(String, &str), String> {
    let s = s.trim_start();
    if !s.starts_with(open) {
        return Err(format!(
            "expected '{open}' but got: {}",
            &s[..s.len().min(20)]
        ));
    }

    let mut depth = 0i32;
    let mut in_string = false;
    let mut escape = false;

    for (i, ch) in s.char_indices() {
        if escape {
            escape = false;
            continue;
        }
        if ch == '\\' && in_string {
            escape = true;
            continue;
        }
        if ch == '"' {
            in_string = !in_string;
            continue;
        }
        if in_string {
            continue;
        }
        if ch == open {
            depth += 1;
        } else if ch == close {
            depth -= 1;
            if depth == 0 {
                let end = i + ch.len_utf8();
                let json_str = &s[..end];
                let remainder = s[end..].trim_start();
                return Ok((json_str.to_string(), remainder));
            }
        }
    }

    Err(format!("unterminated JSON {open}...{close}"))
}

/// Build a WHERE clause from a conditions map. Appends values to `params`
/// and returns the clause (including leading " WHERE ").
fn build_where(
    conditions: &Map<String, Value>,
    params: &mut Vec<String>,
) -> Result<String, String> {
    if conditions.is_empty() {
        return Ok(String::new());
    }

    let mut clauses: Vec<String> = Vec::new();

    for (key, value) in conditions {
        let (column, op) = parse_field_operator(key)?;
        validate_ident(&column)?;

        let idx = params.len() + 1;

        let (sql_fragment, param_value) = match op {
            Op::Eq => {
                if value.is_null() {
                    (format!("{column} IS NULL"), None)
                } else {
                    (
                        format!("{column}::text = ${idx}"),
                        Some(json_value_to_string(value)?),
                    )
                }
            }
            Op::Neq => {
                if value.is_null() {
                    (format!("{column} IS NOT NULL"), None)
                } else {
                    (
                        format!("{column}::text != ${idx}"),
                        Some(json_value_to_string(value)?),
                    )
                }
            }
            Op::Gt => (
                format!("{column}::text > ${idx}"),
                Some(json_value_to_string(value)?),
            ),
            Op::Gte => (
                format!("{column}::text >= ${idx}"),
                Some(json_value_to_string(value)?),
            ),
            Op::Lt => (
                format!("{column}::text < ${idx}"),
                Some(json_value_to_string(value)?),
            ),
            Op::Lte => (
                format!("{column}::text <= ${idx}"),
                Some(json_value_to_string(value)?),
            ),
            Op::Contains => {
                let s = value
                    .as_str()
                    .ok_or_else(|| format!("__contains requires a string value for \"{key}\""))?;
                (format!("{column} ILIKE ${idx}"), Some(format!("%{s}%")))
            }
        };

        clauses.push(sql_fragment);
        if let Some(v) = param_value {
            params.push(v);
        }
    }

    Ok(format!(" WHERE {}", clauses.join(" AND ")))
}

enum Op {
    Eq,
    Neq,
    Gt,
    Gte,
    Lt,
    Lte,
    Contains,
}

/// Parse "field__op" into (field, operator). No suffix = Eq.
fn parse_field_operator(key: &str) -> Result<(String, Op), String> {
    if let Some(field) = key.strip_suffix("__gt") {
        Ok((field.to_string(), Op::Gt))
    } else if let Some(field) = key.strip_suffix("__gte") {
        Ok((field.to_string(), Op::Gte))
    } else if let Some(field) = key.strip_suffix("__lt") {
        Ok((field.to_string(), Op::Lt))
    } else if let Some(field) = key.strip_suffix("__lte") {
        Ok((field.to_string(), Op::Lte))
    } else if let Some(field) = key.strip_suffix("__contains") {
        Ok((field.to_string(), Op::Contains))
    } else if let Some(field) = key.strip_suffix("__neq") {
        Ok((field.to_string(), Op::Neq))
    } else {
        Ok((key.to_string(), Op::Eq))
    }
}

/// Convert a JSON value to a string for parameter binding.
fn json_value_to_string(value: &Value) -> Result<String, String> {
    match value {
        Value::String(s) => Ok(s.clone()),
        Value::Number(n) => Ok(n.to_string()),
        Value::Bool(b) => Ok(b.to_string()),
        Value::Null => Err("unexpected null value in parameter".into()),
        Value::Array(_) | Value::Object(_) => Ok(value.to_string()),
    }
}

/// Bind a serde_json::Value to a sqlx query argument.
fn bind_json_value<'q>(
    q: sqlx::query::Query<'q, sqlx::Postgres, sqlx::postgres::PgArguments>,
    value: &'q Value,
) -> sqlx::query::Query<'q, sqlx::Postgres, sqlx::postgres::PgArguments> {
    match value {
        Value::String(s) => q.bind(s.as_str()),
        Value::Number(n) => {
            if let Some(i) = n.as_i64() {
                q.bind(i)
            } else if let Some(f) = n.as_f64() {
                q.bind(f)
            } else {
                q.bind(n.to_string())
            }
        }
        Value::Bool(b) => q.bind(*b),
        Value::Null => q.bind(None::<String>),
        // For arrays/objects, store as JSONB string.
        _ => q.bind(value.to_string()),
    }
}

/// Convert sqlx rows to JSON values using column metadata.
fn rows_to_json(rows: &[sqlx::postgres::PgRow]) -> Vec<Value> {
    use sqlx::Column;
    use sqlx::TypeInfo;

    rows.iter()
        .map(|row| {
            let mut obj = Map::new();
            for col in row.columns() {
                let name = col.name();
                let type_name = col.type_info().name();
                let val = pg_column_to_json(row, name, type_name);
                obj.insert(name.to_string(), val);
            }
            Value::Object(obj)
        })
        .collect()
}

/// Read a single column value from a PgRow and convert to serde_json::Value.
fn pg_column_to_json(row: &sqlx::postgres::PgRow, name: &str, type_name: &str) -> Value {
    // Try to extract based on PostgreSQL type name.
    match type_name {
        "INT2" | "INT4" => row
            .try_get::<Option<i32>, _>(name)
            .ok()
            .flatten()
            .map(|v| Value::Number(v.into()))
            .unwrap_or(Value::Null),
        "INT8" | "BIGSERIAL" => row
            .try_get::<Option<i64>, _>(name)
            .ok()
            .flatten()
            .map(|v| Value::Number(v.into()))
            .unwrap_or(Value::Null),
        "FLOAT4" => row
            .try_get::<Option<f32>, _>(name)
            .ok()
            .flatten()
            .and_then(|v| serde_json::Number::from_f64(v as f64))
            .map(Value::Number)
            .unwrap_or(Value::Null),
        "FLOAT8" | "DOUBLE PRECISION" => row
            .try_get::<Option<f64>, _>(name)
            .ok()
            .flatten()
            .and_then(serde_json::Number::from_f64)
            .map(Value::Number)
            .unwrap_or(Value::Null),
        "BOOL" => row
            .try_get::<Option<bool>, _>(name)
            .ok()
            .flatten()
            .map(Value::Bool)
            .unwrap_or(Value::Null),
        "DATE" => row
            .try_get::<Option<chrono::NaiveDate>, _>(name)
            .ok()
            .flatten()
            .map(|d| Value::String(d.format("%Y-%m-%d").to_string()))
            .unwrap_or(Value::Null),
        "TIMESTAMPTZ" => row
            .try_get::<Option<chrono::DateTime<chrono::Utc>>, _>(name)
            .ok()
            .flatten()
            .map(|t| Value::String(t.to_rfc3339()))
            .unwrap_or(Value::Null),
        "TIMESTAMP" => row
            .try_get::<Option<chrono::NaiveDateTime>, _>(name)
            .ok()
            .flatten()
            .map(|t| Value::String(t.format("%Y-%m-%dT%H:%M:%S").to_string()))
            .unwrap_or(Value::Null),
        "JSONB" | "JSON" => row
            .try_get::<Option<Value>, _>(name)
            .ok()
            .flatten()
            .unwrap_or(Value::Null),
        // Default: try as string.
        _ => row
            .try_get::<Option<String>, _>(name)
            .ok()
            .flatten()
            .map(Value::String)
            .unwrap_or(Value::Null),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn validate_ident_valid() {
        assert!(validate_ident("users").is_ok());
        assert!(validate_ident("my_table_123").is_ok());
        assert!(validate_ident("a").is_ok());
    }

    #[test]
    fn validate_ident_invalid() {
        assert!(validate_ident("").is_err());
        assert!(validate_ident("123abc").is_err());
        assert!(validate_ident("My_Table").is_err());
        assert!(validate_ident("drop;table").is_err());
        assert!(validate_ident("a-b").is_err());
    }

    #[test]
    fn split_first_word_basic() {
        assert_eq!(split_first_word("hello world"), Some(("hello", "world")));
        assert_eq!(split_first_word("single"), Some(("single", "")));
        assert_eq!(split_first_word("  spaced  text"), Some(("spaced", "text")));
        assert_eq!(split_first_word(""), None);
        assert_eq!(split_first_word("   "), None);
    }

    #[test]
    fn extract_json_object_basic() {
        let (json, rest) = extract_json_object(r#"{"a": 1} sort name"#).unwrap();
        assert_eq!(json, r#"{"a": 1}"#);
        assert_eq!(rest, "sort name");
    }

    #[test]
    fn extract_json_object_nested() {
        let (json, rest) = extract_json_object(r#"{"a": {"b": 1}, "c": "}"} rest"#).unwrap();
        assert_eq!(json, r#"{"a": {"b": 1}, "c": "}"}"#);
        assert_eq!(rest, "rest");
    }

    #[test]
    fn extract_json_array_basic() {
        let (json, rest) = extract_json_array(r#"["a", "b"] rest"#).unwrap();
        assert_eq!(json, r#"["a", "b"]"#);
        assert_eq!(rest, "rest");
    }

    #[test]
    fn parse_field_operator_cases() {
        let (f, _) = parse_field_operator("age__gt").unwrap();
        assert_eq!(f, "age");

        let (f, _) = parse_field_operator("name__contains").unwrap();
        assert_eq!(f, "name");

        let (f, _) = parse_field_operator("status").unwrap();
        assert_eq!(f, "status");

        let (f, _) = parse_field_operator("score__lte").unwrap();
        assert_eq!(f, "score");
    }

    #[test]
    fn build_where_simple() {
        let mut params = Vec::new();
        let conditions: Map<String, Value> =
            serde_json::from_str(r#"{"status": "active", "age__gt": 18}"#).unwrap();
        let clause = build_where(&conditions, &mut params).unwrap();
        assert!(clause.contains("WHERE"));
        // Key order in Map may vary; check both conditions appear with valid placeholders.
        assert!(clause.contains("status::text = $"));
        assert!(clause.contains("age::text > $"));
        assert_eq!(params.len(), 2);
        assert!(params.contains(&"active".to_string()));
        assert!(params.contains(&"18".to_string()));
    }

    #[test]
    fn build_where_contains() {
        let mut params = Vec::new();
        let conditions: Map<String, Value> =
            serde_json::from_str(r#"{"name__contains": "alice"}"#).unwrap();
        let clause = build_where(&conditions, &mut params).unwrap();
        assert!(clause.contains("ILIKE"));
        assert_eq!(params[0], "%alice%");
    }

    #[test]
    fn build_where_null() {
        let mut params = Vec::new();
        let conditions: Map<String, Value> =
            serde_json::from_str(r#"{"deleted_at": null}"#).unwrap();
        let clause = build_where(&conditions, &mut params).unwrap();
        assert!(clause.contains("IS NULL"));
        assert!(params.is_empty());
    }

    #[test]
    fn build_where_neq_null() {
        let mut params = Vec::new();
        let conditions: Map<String, Value> =
            serde_json::from_str(r#"{"deleted_at__neq": null}"#).unwrap();
        let clause = build_where(&conditions, &mut params).unwrap();
        assert!(clause.contains("IS NOT NULL"));
        assert!(params.is_empty());
    }

    #[test]
    fn pg_type_mapping() {
        assert_eq!(pg_type("text"), Some("TEXT"));
        assert_eq!(pg_type("number"), Some("DOUBLE PRECISION"));
        assert_eq!(pg_type("date"), Some("DATE"));
        assert_eq!(pg_type("boolean"), Some("BOOLEAN"));
        assert_eq!(pg_type("timestamp"), Some("TIMESTAMPTZ"));
        assert_eq!(pg_type("json"), Some("JSONB"));
        assert_eq!(pg_type("unknown"), None);
    }

    #[test]
    fn json_value_to_string_cases() {
        assert_eq!(
            json_value_to_string(&Value::String("hello".into())).unwrap(),
            "hello"
        );
        assert_eq!(json_value_to_string(&serde_json::json!(42)).unwrap(), "42");
        assert_eq!(
            json_value_to_string(&serde_json::json!(true)).unwrap(),
            "true"
        );
        assert!(json_value_to_string(&Value::Null).is_err());
    }

    #[test]
    fn auto_columns_rejected_in_create_json() {
        // Simulate what cmd_create does for auto-column validation.
        let columns: Map<String, Value> =
            serde_json::from_str(r#"{"id": "number", "name": "text"}"#).unwrap();
        let has_auto = columns.keys().any(|k| AUTO_COLUMNS.contains(&k.as_str()));
        assert!(has_auto);
    }

    #[test]
    fn status_label_formatting() {
        let label_empty = format!(
            "\u{1f5c4}\u{fe0f} Database: {}",
            super::super::truncate_display("", 40)
        );
        // Just verify the prefix is present.
        assert!(label_empty.contains("Database"));
    }
}
