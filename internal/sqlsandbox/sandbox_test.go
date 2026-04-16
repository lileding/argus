package sqlsandbox

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const testPrefix = "argus_"

func TestRewrite_Accepts(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// want is a substring (we don't compare deparsed SQL exactly since
		// the deparser normalizes whitespace / quoting).
		wants []string
		// mustNotContain is helpful for negative assertions (e.g. alias not
		// prefixed).
		mustNotContain []string
	}{
		{
			name:  "simple SELECT",
			in:    "SELECT * FROM food_log",
			wants: []string{"argus_food_log"},
		},
		{
			name:  "INSERT",
			in:    "INSERT INTO food_log (a) VALUES (1)",
			wants: []string{"argus_food_log"},
		},
		{
			name:  "UPDATE WHERE",
			in:    "UPDATE food_log SET a = 1 WHERE b = 2",
			wants: []string{"argus_food_log"},
		},
		{
			name:  "DELETE",
			in:    "DELETE FROM food_log WHERE a = 1",
			wants: []string{"argus_food_log"},
		},
		{
			name:  "JOIN 2 tables",
			in:    "SELECT * FROM a JOIN b ON a.x = b.x",
			wants: []string{"argus_a", "argus_b"},
		},
		{
			name:  "self-join",
			in:    "SELECT * FROM users x JOIN users y ON x.p = y.q",
			wants: []string{"argus_users"},
		},
		{
			name:           "aliased table",
			in:             "SELECT f.x FROM food_log f",
			wants:          []string{"argus_food_log"},
			mustNotContain: []string{"argus_f ", "argus_f."},
		},
		{
			name:  "subquery",
			in:    "SELECT * FROM (SELECT x FROM a) t",
			wants: []string{"argus_a"},
		},
		{
			name:  "CTE not prefixed",
			in:    "WITH cte AS (SELECT x FROM a) SELECT * FROM cte",
			wants: []string{"argus_a"},
			// cte itself must stay unprefixed.
			mustNotContain: []string{"argus_cte"},
		},
		{
			name:  "Nested CTE",
			in:    "WITH o AS (WITH i AS (SELECT 1 AS x) SELECT * FROM i) SELECT * FROM o",
			wants: []string{}, // all references are to CTE names
			mustNotContain: []string{
				"argus_o",
				"argus_i",
			},
		},
		{
			name:  "CREATE TABLE",
			in:    "CREATE TABLE food_log (id serial PRIMARY KEY, kcal int)",
			wants: []string{"argus_food_log"},
		},
		{
			name:  "CREATE INDEX prefixes both",
			in:    "CREATE INDEX idx_f ON food_log (id)",
			wants: []string{"argus_idx_f", "argus_food_log"},
		},
		{
			name:  "ALTER TABLE",
			in:    "ALTER TABLE food_log ADD COLUMN c text",
			wants: []string{"argus_food_log"},
		},
		{
			name:  "DROP TABLE",
			in:    "DROP TABLE food_log",
			wants: []string{"argus_food_log"},
		},
		{
			name:  "DROP INDEX",
			in:    "DROP INDEX idx_f",
			wants: []string{"argus_idx_f"},
		},
		{
			name:  "DROP IF EXISTS",
			in:    "DROP TABLE IF EXISTS food_log",
			wants: []string{"argus_food_log"},
		},
		{
			name:  "TRUNCATE multi",
			in:    "TRUNCATE a, b, c",
			wants: []string{"argus_a", "argus_b", "argus_c"},
		},
		{
			name:  "ON CONFLICT",
			in:    "INSERT INTO food_log (id, kcal) VALUES (1, 500) ON CONFLICT (id) DO UPDATE SET kcal = EXCLUDED.kcal",
			wants: []string{"argus_food_log"},
		},
		{
			name:  "RETURNING",
			in:    "INSERT INTO food_log (a) VALUES (1) RETURNING id",
			wants: []string{"argus_food_log"},
		},
		{
			name:  "generate_series (set-returning fn, not RangeVar)",
			in:    "SELECT * FROM generate_series(1, 10)",
			wants: []string{"generate_series"},
			// must not prefix the function name
			mustNotContain: []string{"argus_generate"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Rewrite(tc.in, testPrefix)
			if err != nil {
				t.Fatalf("Rewrite returned error: %v\ninput: %s", err, tc.in)
			}
			for _, w := range tc.wants {
				if !strings.Contains(out, w) {
					t.Errorf("expected output to contain %q\ngot: %s", w, out)
				}
			}
			for _, bad := range tc.mustNotContain {
				if strings.Contains(out, bad) {
					t.Errorf("expected output NOT to contain %q\ngot: %s", bad, out)
				}
			}
		})
	}
}

func TestRewrite_Rejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// errContains is a substring the error message must contain.
		errContains string
	}{
		{
			name:        "schema-qualified in SELECT",
			in:          "SELECT * FROM public.food_log",
			errContains: "schema-qualified",
		},
		{
			name:        "information_schema",
			in:          "SELECT * FROM information_schema.tables",
			errContains: "schema-qualified",
		},
		{
			name:        "pg_catalog",
			in:          "SELECT * FROM pg_catalog.pg_tables",
			errContains: "schema-qualified",
		},
		{
			name:        "multi-statement",
			in:          "SELECT 1; DROP TABLE messages",
			errContains: "single SQL statement",
		},
		{
			name:        "pre-prefixed in SELECT",
			in:          "SELECT * FROM argus_food_log",
			errContains: "argus_",
		},
		{
			name:        "pre-prefixed in CREATE",
			in:          "CREATE TABLE argus_x (a int)",
			errContains: "argus_",
		},
		{
			name:        "pre-prefixed in ALTER",
			in:          "ALTER TABLE argus_food_log ADD COLUMN c int",
			errContains: "argus_",
		},
		{
			name:        "pre-prefixed in DROP",
			in:          "DROP TABLE argus_food_log",
			errContains: "argus_",
		},
		{
			name:        "pre-prefixed index name",
			in:          "CREATE INDEX argus_idx ON food_log (id)",
			errContains: "argus_",
		},
		{
			name:        "schema-qualified DROP",
			in:          "DROP TABLE public.food_log",
			errContains: "schema-qualified",
		},
		{
			name:        "parse error",
			in:          "SELECT * FROM WHERE x = 1",
			errContains: "sql parse error",
		},
		{
			name:        "empty",
			in:          "   ",
			errContains: "empty SQL",
		},
		{
			name:        "DROP SCHEMA",
			in:          "DROP SCHEMA public",
			errContains: "not permitted",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Rewrite(tc.in, testPrefix)
			if err == nil {
				t.Fatalf("expected error, got none\ninput: %s", tc.in)
			}
			if !strings.Contains(err.Error(), tc.errContains) {
				t.Errorf("error %q did not contain %q", err.Error(), tc.errContains)
			}
		})
	}
}

func TestStripPrefix(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{
			in:   `relation "argus_food_log" does not exist`,
			want: `relation "food_log" does not exist`,
		},
		{
			in:   `column "argus_users"."kcal" does not exist`,
			want: `column "users"."kcal" does not exist`,
		},
		{
			// "argus_" followed by non-identifier char: leave alone.
			in:   `no prefix here: argus_`,
			want: `no prefix here: argus_`,
		},
		{
			// multiple occurrences
			in:   `argus_a and argus_b are both gone`,
			want: `a and b are both gone`,
		},
		{
			// case-insensitive
			in:   `ARGUS_FOO vanished`,
			want: `FOO vanished`,
		},
		{
			in:   ``,
			want: ``,
		},
	}
	for _, tc := range cases {
		got := StripPrefix(tc.in, testPrefix)
		if got != tc.want {
			t.Errorf("StripPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestMigrationsNoPrefixedTables enforces invariant #1: no Argus system
// table name begins with the sandbox prefix. Future migrations that
// accidentally reserve a model name will fail this guard.
func TestMigrationsNoPrefixedTables(t *testing.T) {
	migrationsDir := filepath.Join("..", "store", "migrations")
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	// Match CREATE TABLE [IF NOT EXISTS] argus_identifier (case-insensitive).
	re := regexp.MustCompile(`(?i)create\s+table\s+(if\s+not\s+exists\s+)?argus_\w+`)

	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(migrationsDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if matches := re.FindAllString(string(body), -1); len(matches) > 0 {
			t.Errorf("migration %s reserves a model-namespace name: %v", e.Name(), matches)
			found += len(matches)
		}
	}
	if found == 0 {
		t.Logf("invariant holds: no %q-prefixed system tables", testPrefix)
	}
}
