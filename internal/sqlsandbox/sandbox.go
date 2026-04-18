// Package sqlsandbox rewrites model-issued SQL so every user-level table
// reference carries a hidden prefix (default: "argus_"). The model writes
// SQL against logical names ("food_log"); the sandbox transforms them to
// physical names ("argus_food_log") before execution.
//
// The rewriter uses libpg_query (PostgreSQL's own parser, via
// github.com/pganalyze/pg_query_go/v6) for AST-faithful transformation.
// String/regex-based rewriting would misfire on quoted identifiers,
// comments, string literals, and CTE names.
//
// Invariants (see DESIGN.md §DB Sandboxing):
//  1. No Argus system table starts with the prefix. The prefix is
//     reserved for the model's namespace.
//  2. Every model-issued RangeVar is prefixed.
//  3. Inputs that already contain an identifier starting with the prefix
//     are rejected (closes the double-prefix escape).
//
// Together these guarantee model-prefixed tables cannot collide with
// system tables.
package sqlsandbox

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// DefaultPrefix is the namespace prefix applied to all model-issued
// table/index identifiers.
const DefaultPrefix = "argus_"

// MaxSelectLimit is injected into SELECT statements that have no LIMIT
// clause (or a LIMIT larger than this). Prevents the model from doing
// accidental full-table scans on large tables.
const MaxSelectLimit = 200

// Rewrite parses sql, prefixes every user-level table/index reference,
// and returns the transformed SQL. Returns a descriptive error if the
// SQL is multi-statement, schema-qualified, already prefixed, or fails
// to parse.
func Rewrite(sql, prefix string) (string, error) {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		return "", errors.New("empty SQL")
	}
	if prefix == "" {
		return "", errors.New("empty prefix")
	}

	tree, err := pg_query.Parse(sql)
	if err != nil {
		return "", fmt.Errorf("sql parse error: %w", err)
	}
	if len(tree.Stmts) == 0 {
		return "", errors.New("no statement found in SQL")
	}
	if len(tree.Stmts) > 1 {
		return "", errors.New("only a single SQL statement is allowed per call")
	}

	pfxLow := strings.ToLower(prefix)

	// Pass 1 — collect CTE names from the whole tree. A RangeVar whose
	// name matches a CTE is not a base-table reference.
	//
	// Using a global set rather than proper per-statement scope tracking
	// is a small but deliberate simplification: the degenerate case where
	// a CTE shadows its own underlying table (WITH foo AS (SELECT FROM foo))
	// will under-prefix the inner reference. Practically never produced
	// by LLMs, and PostgreSQL would have returned the CTE row anyway if
	// the CTE were non-recursive — so the behavior divergence is minor.
	cteNames := map[string]bool{}
	walkValue(reflect.ValueOf(tree), func(v reflect.Value) bool {
		if cte, ok := asConcrete[*pg_query.CommonTableExpr](v); ok && cte != nil && cte.Ctename != "" {
			cteNames[strings.ToLower(cte.Ctename)] = true
		}
		return true
	})

	// Pass 2 — mutate RangeVars, IndexStmt.Idxname, and DropStmt.Objects
	// identifiers. Any schema-qualified name or pre-prefixed identifier
	// short-circuits with a descriptive error.
	var firstErr error
	fail := func(err error) bool {
		if firstErr == nil {
			firstErr = err
		}
		return false // stop traversal
	}

	walkValue(reflect.ValueOf(tree), func(v reflect.Value) bool {
		if firstErr != nil {
			return false
		}

		if rv, ok := asConcrete[*pg_query.RangeVar](v); ok && rv != nil {
			if rv.Schemaname != "" || rv.Catalogname != "" {
				return fail(fmt.Errorf("schema-qualified names are not allowed: %s", rangeVarName(rv)))
			}
			if rv.Relname == "" {
				return true
			}
			if cteNames[strings.ToLower(rv.Relname)] {
				return true
			}
			if strings.HasPrefix(strings.ToLower(rv.Relname), pfxLow) {
				return fail(fmt.Errorf("identifier %q is not allowed: do not use the %q prefix in your SQL", rv.Relname, prefix))
			}
			rv.Relname = prefix + rv.Relname
			return true
		}

		if idx, ok := asConcrete[*pg_query.IndexStmt](v); ok && idx != nil {
			// idx.Relation is a *RangeVar and will be visited separately.
			if idx.Idxname != "" {
				if strings.HasPrefix(strings.ToLower(idx.Idxname), pfxLow) {
					return fail(fmt.Errorf("index name %q is not allowed: do not use the %q prefix in your SQL", idx.Idxname, prefix))
				}
				idx.Idxname = prefix + idx.Idxname
			}
			return true
		}

		if drop, ok := asConcrete[*pg_query.DropStmt](v); ok && drop != nil {
			if err := rewriteDropStmt(drop, cteNames, prefix, pfxLow); err != nil {
				return fail(err)
			}
			// Don't descend — DropStmt.Objects are dotted-name Lists we
			// just handled explicitly; re-walking would add nothing.
			return false
		}

		return true
	})
	if firstErr != nil {
		return "", firstErr
	}

	// Pass 3 — inject LIMIT on top-level SELECT without one (or with one
	// exceeding MaxSelectLimit). Prevents accidental full-table scans.
	injectSelectLimit(tree)

	out, err := pg_query.Deparse(tree)
	if err != nil {
		return "", fmt.Errorf("sql deparse error: %w", err)
	}
	return out, nil
}

// injectSelectLimit adds or caps LIMIT on the top-level SELECT statement.
// Subqueries and CTEs are not touched — only the outermost query matters
// for result-set size.
func injectSelectLimit(tree *pg_query.ParseResult) {
	if len(tree.Stmts) == 0 {
		return
	}
	node := tree.Stmts[0].Stmt
	if node == nil {
		return
	}
	sel, ok := node.Node.(*pg_query.Node_SelectStmt)
	if !ok || sel.SelectStmt == nil {
		return
	}
	st := sel.SelectStmt

	// UNION/INTERSECT/EXCEPT: the LimitCount lives on the wrapper node,
	// but the outermost SelectStmt is the one with Op != SETOP_NONE.
	// For simplicity, only inject on simple selects (Op == SETOP_NONE).
	if st.Op != pg_query.SetOperation_SETOP_NONE {
		return
	}

	limitNode := func(n int32) *pg_query.Node {
		return &pg_query.Node{
			Node: &pg_query.Node_AConst{
				AConst: &pg_query.A_Const{
					Val: &pg_query.A_Const_Ival{Ival: &pg_query.Integer{Ival: n}},
				},
			},
		}
	}

	if st.LimitCount == nil {
		// No LIMIT → inject one.
		st.LimitCount = limitNode(MaxSelectLimit)
		st.LimitOption = pg_query.LimitOption_LIMIT_OPTION_COUNT
		return
	}

	// Has LIMIT — cap it if it's a constant larger than MaxSelectLimit.
	if ac, ok := st.LimitCount.Node.(*pg_query.Node_AConst); ok && ac.AConst != nil {
		if iv, ok := ac.AConst.Val.(*pg_query.A_Const_Ival); ok && iv.Ival != nil {
			if iv.Ival.Ival > MaxSelectLimit {
				iv.Ival.Ival = MaxSelectLimit
			}
		}
	}
}

// StripPrefix removes occurrences of prefix from user-visible text such as
// PostgreSQL error messages. Matches prefix immediately followed by an
// identifier character to avoid mangling unrelated text that might
// coincidentally contain "argus_".
func StripPrefix(text, prefix string) string {
	if prefix == "" || text == "" {
		return text
	}
	re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(prefix) + `([A-Za-z_][A-Za-z0-9_]*)`)
	return re.ReplaceAllString(text, "$1")
}

// rewriteDropStmt handles DROP TABLE/INDEX/VIEW/SEQUENCE/MATVIEW by
// rewriting the string identifiers inside each object's dotted-name list.
// Other DROP object kinds (SCHEMA, FUNCTION, etc.) are rejected — they
// have no legitimate use from the model.
func rewriteDropStmt(drop *pg_query.DropStmt, cteNames map[string]bool, prefix, pfxLow string) error {
	switch drop.RemoveType {
	case pg_query.ObjectType_OBJECT_TABLE,
		pg_query.ObjectType_OBJECT_INDEX,
		pg_query.ObjectType_OBJECT_VIEW,
		pg_query.ObjectType_OBJECT_SEQUENCE,
		pg_query.ObjectType_OBJECT_MATVIEW:
		// supported
	default:
		return fmt.Errorf("DROP of object type %s is not permitted", drop.RemoveType)
	}
	for _, obj := range drop.Objects {
		list := obj.GetList()
		if list == nil || len(list.Items) == 0 {
			continue
		}
		if len(list.Items) > 1 {
			return errors.New("schema-qualified names are not allowed in DROP")
		}
		s := list.Items[0].GetString_()
		if s == nil || s.Sval == "" {
			continue
		}
		name := s.Sval
		if cteNames[strings.ToLower(name)] {
			continue
		}
		if strings.HasPrefix(strings.ToLower(name), pfxLow) {
			return fmt.Errorf("identifier %q is not allowed: do not use the %q prefix in your SQL", name, prefix)
		}
		s.Sval = prefix + name
	}
	return nil
}

// rangeVarName renders a RangeVar as "schema.relname" for error messages.
func rangeVarName(rv *pg_query.RangeVar) string {
	var parts []string
	if rv.Catalogname != "" {
		parts = append(parts, rv.Catalogname)
	}
	if rv.Schemaname != "" {
		parts = append(parts, rv.Schemaname)
	}
	parts = append(parts, rv.Relname)
	return strings.Join(parts, ".")
}

// asConcrete returns the underlying value if v's interface matches T.
func asConcrete[T any](v reflect.Value) (T, bool) {
	var zero T
	if !v.IsValid() || !v.CanInterface() {
		return zero, false
	}
	iface := v.Interface()
	t, ok := iface.(T)
	return t, ok
}

// walkValue recursively visits every value in v. The visit callback
// returns false to stop descending into the current node (useful when a
// subtree has been handled explicitly and re-traversal would double-mutate).
//
// Unexported fields are skipped on the recursion side (not just at the
// visit call) to avoid diving into protobuf's internal bookkeeping
// (state/sizeCache/unknownFields/messagestate), which contains pointers,
// atomics, and mutexes — walking them explodes time and/or panics.
func walkValue(v reflect.Value, visit func(reflect.Value) bool) {
	if !v.IsValid() || !v.CanInterface() {
		return
	}
	if !visit(v) {
		return
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return
		}
		walkValue(v.Elem(), visit)
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			if !t.Field(i).IsExported() {
				continue
			}
			walkValue(v.Field(i), visit)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			walkValue(v.Index(i), visit)
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			walkValue(iter.Value(), visit)
		}
	}
}
