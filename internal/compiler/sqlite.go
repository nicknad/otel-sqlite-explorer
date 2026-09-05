// Package compiler translates DSL Query objects into parameterized SQLite
// statements.  This is the ONLY package that constructs SQL strings — no other
// package imports database/sql or builds SQL directly.
package compiler

import (
	"errors"
	"fmt"
	"strings"

	"log-explorer/internal/dsl"
)

// CompiledQuery holds a safe, parameterized SQL string and its arguments.
// Fields lists the DSL field names in SELECT order (nil means all default
// fields); it is used by the db package to scan rows into LogRow.
type CompiledQuery struct {
	SQL    string
	Args   []any
	Fields []string
}

// fieldMap translates DSL field names to SQL column expressions.
// The values match the column names exposed by the `logs` view in the
// otel-sqlite schema. BLOB columns (trace_id, span_id) are wrapped with
// hex() at the expression level — see colExpr and selectCols.
var fieldMap = map[string]string{
	"id":              "id",
	"timestamp":       "timestamp_ns",
	"severity":        "severity_text",
	"severity_number": "severity_number",
	"service_name":    "service_name",
	"trace_id":        "trace_id",
	"span_id":         "span_id",
	"body":            "body",
	"attributes_json": "attributes_json",
}

// isBlobField reports whether a DSL field / view column is a BLOB that must
// be hex-encoded (lower(hex(...))) so string comparisons and scanning into Go
// strings work correctly.
func isBlobField(name string) bool {
	return name == "trace_id" || name == "span_id"
}

// colExpr returns the SQL column expression for a DSL field, optionally
// qualified with the logs table alias for FTS5 JOIN disambiguation.
// BLOB columns (trace_id, span_id) are wrapped with hex() by isBlobField.
func colExpr(field string, qualify bool) (string, error) {
	col, ok := fieldMap[field]
	if !ok {
		return "", fmt.Errorf("unsupported field: %s", field)
	}
	if qualify {
		col = "logs." + col
	}
	if isBlobField(field) {
		col = "lower(hex(" + col + "))"
	}
	return col, nil
}

// logColumns defines the column names from the `logs` view, in LogRow
// scan order. These are used to build the SELECT list; BLOB columns
// (trace_id, span_id) are wrapped with hex() by selectCols.
var logColumns = []string{
	"id", "timestamp_ns", "severity_text", "severity_number", "service_name", "trace_id", "span_id", "body", "attributes_json",
}

// DefaultFields lists the DSL field names returned by a "*" select, in LogRow
// scan order. The db package scans rows against this list when a query does
// not restrict its select list.
var DefaultFields = []string{
	"id", "timestamp", "severity", "severity_number", "service_name", "trace_id", "span_id", "body", "attributes_json",
}

// Constant SQL fragments.
const fromLogs = "FROM logs"

// CompileGetByID builds a parameterized SELECT for a single log row by id.
func CompileGetByID(id int64) *CompiledQuery {
	return &CompiledQuery{
		SQL:    "SELECT " + selectCols(false) + " " + fromLogs + " WHERE id = ? LIMIT 1",
		Args:   []any{id},
		Fields: DefaultFields,
	}
}

// selectCols returns the comma-separated column list, optionally qualified
// with the `logs.` table alias. Qualification is required when the FTS5
// index is joined in, because logs_fts also exposes body/service_name.
// BLOB columns (trace_id, span_id) are wrapped with hex() by isBlobField so
// they scan as hex-encoded text strings in Go.
func selectCols(qualify bool) string {
	parts := make([]string, len(logColumns))
	for i, c := range logColumns {
		expr := c
		if qualify {
			expr = "logs." + expr
		}
		if isBlobField(c) {
			expr = "lower(hex(" + expr + "))"
		}
		parts[i] = expr
	}
	return strings.Join(parts, ", ")
}

// selectList turns a Query.Select value ("*" or a comma-separated DSL field
// list) into the SQL column list plus the DSL field names in SELECT order.
// Qualification and BLOB hex-wrapping are applied per column. An empty or "*"
// select expands to the default column set.
func selectList(sel string, qualify bool) (string, []string, error) {
	if strings.TrimSpace(sel) == "" {
		sel = "*"
	}
	fields := make([]string, 0, len(logColumns))
	cols := make([]string, 0, len(logColumns))
	for part := range strings.SplitSeq(sel, ",") {
		f := strings.TrimSpace(part)
		if f == "" {
			continue
		}
		if f == "*" {
			return selectCols(qualify), DefaultFields, nil
		}
		col, err := colExpr(f, qualify)
		if err != nil {
			return "", nil, err
		}
		cols = append(cols, col)
		fields = append(fields, f)
	}
	return strings.Join(cols, ", "), fields, nil
}

// Compile translates a validated, normalized DSL Query into a parameterized
// SQLite SELECT statement.
//
// When the WHERE tree contains a MatchExpr (FTS5 full-text search) the query
// uses a JOIN against logs_fts so the MATCH predicate is evaluated against the
// full-text index rather than as a full table scan. All logs column references
// are then qualified with `logs.` to avoid ambiguity with logs_fts columns.
//
// The output is deterministic: identical input → identical SQL and arg order.
func Compile(q *dsl.Query) (*CompiledQuery, error) {
	matched := dsl.HasMatch(q.Where)
	c := &exprCompiler{qualify: matched}

	var b strings.Builder
	var args []any

	// ---- SELECT ----
	sel, fields, err := selectList(q.Select, matched)
	if err != nil {
		return nil, fmt.Errorf("select: %w", err)
	}
	b.WriteString("SELECT ")
	b.WriteString(sel)

	// ---- FROM (with optional FTS5 join) ----
	b.WriteString(" ")
	b.WriteString(fromLogs)
	if matched {
		b.WriteString(" JOIN logs_fts ON logs.id = logs_fts.rowid")
	}

	// ---- WHERE ----
	if q.Where != nil {
		whereSQL, whereArgs, err := c.compileExpr(q.Where)
		if err != nil {
			return nil, fmt.Errorf("compile where: %w", err)
		}
		b.WriteString(" WHERE ")
		b.WriteString(whereSQL)
		args = append(args, whereArgs...)
	}

	// ---- ORDER BY ----
	switch {
	case len(q.Sort) > 0:
		b.WriteString(" ORDER BY ")
		parts := make([]string, 0, len(q.Sort))
		for _, s := range q.Sort {
			col, err := colExpr(s.Field, matched)
			if err != nil {
				return nil, fmt.Errorf("unsupported sort field: %s: %w", s.Field, err)
			}
			dir := "ASC"
			if s.Desc {
				dir = "DESC"
			}
			parts = append(parts, fmt.Sprintf("%s %s", col, dir))
		}
		b.WriteString(strings.Join(parts, ", "))
	case matched:
		// No explicit sort + full-text match → rank by relevance (bm25).
		// bm25 returns more-negative scores for better matches, so ASC
		// puts the most relevant rows first.
		b.WriteString(" ORDER BY bm25(logs_fts) ASC")
	default:
		// Defense in depth: callers that bypass Normalize still get a
		// deterministic order so LIMIT/OFFSET pagination is stable.
		col, _ := colExpr("timestamp", matched) // known field, never errors
		b.WriteString(" ORDER BY " + col + " DESC")
	}

	// ---- LIMIT / OFFSET ----
	// Clamped defensively; validated callers always pass through Normalize,
	// which applies the same defaults. The MaxLimit+1 headroom preserves the
	// API layer's limit+1 "has next page" probe.
	limit := q.Limit
	if limit <= 0 {
		limit = dsl.DefaultLimit
	}
	if limit > dsl.MaxLimit+1 {
		limit = dsl.MaxLimit + 1
	}
	offset := max(q.Offset, 0)
	b.WriteString(" LIMIT ? OFFSET ?")
	args = append(args, limit, offset)

	return &CompiledQuery{SQL: b.String(), Args: args, Fields: fields}, nil
}

// exprCompiler carries compilation context (column qualification) through the
// expression tree walk.
type exprCompiler struct {
	qualify bool
}

// col returns the SQL column reference for a DSL field, qualified when needed.
// BLOB fields (trace_id, span_id) are wrapped with hex() so string comparisons
// against the view's BLOB columns work correctly.
func (c *exprCompiler) col(field string) (string, error) {
	return colExpr(field, c.qualify)
}

// compileExpr walks an expression tree and returns SQL + args.
func (c *exprCompiler) compileExpr(e dsl.Expr) (sql string, args []any, err error) {
	switch expr := e.(type) {
	case dsl.BinaryExpr:
		return c.compileBinary(&expr)
	case dsl.LogicalExpr:
		return c.compileLogical(&expr)
	case dsl.MatchExpr:
		// FTS5 MATCH against the joined index. The JOIN is emitted by Compile
		// whenever a MatchExpr is present in the tree.
		return "logs_fts MATCH ?", []any{expr.Query}, nil
	default:
		return "", nil, fmt.Errorf("unsupported expression type %T", e)
	}
}

func (c *exprCompiler) compileBinary(e *dsl.BinaryExpr) (sql string, args []any, err error) {
	col, err := c.col(e.Field)
	if err != nil {
		return "", nil, err
	}

	// blobArg lowercases string arguments for BLOB (hex-encoded) fields so
	// filters match regardless of the input's hex case. The compiler emits
	// lower(hex(col)), hence the argument must be lowercase too.
	blobArg := func(v any) any {
		if isBlobField(e.Field) {
			if s, ok := v.(string); ok {
				return strings.ToLower(s)
			}
		}
		return v
	}

	switch e.Op {
	case dsl.OpEq:
		return col + " = ?", []any{blobArg(valueToAny(&e.Value))}, nil
	case dsl.OpNe:
		return col + " != ?", []any{blobArg(valueToAny(&e.Value))}, nil
	case dsl.OpGt:
		return col + " > ?", []any{blobArg(valueToAny(&e.Value))}, nil
	case dsl.OpGte:
		return col + " >= ?", []any{blobArg(valueToAny(&e.Value))}, nil
	case dsl.OpLt:
		return col + " < ?", []any{blobArg(valueToAny(&e.Value))}, nil
	case dsl.OpLte:
		return col + " <= ?", []any{blobArg(valueToAny(&e.Value))}, nil
	case dsl.OpContains:
		val := valueToAny(&e.Value)
		str, ok := val.(string)
		if !ok {
			return "", nil, errors.New("contains requires a string value")
		}
		if isBlobField(e.Field) {
			str = strings.ToLower(str)
		}
		return col + " LIKE ? ESCAPE '\\'", []any{"%" + escapeLike(str) + "%"}, nil
	case dsl.OpBetween:
		if e.Value.Min == nil || e.Value.Max == nil {
			return "", nil, errors.New("between requires min and max")
		}
		return col + " BETWEEN ? AND ?",
			[]any{blobArg(valueToAny(e.Value.Min)), blobArg(valueToAny(e.Value.Max))}, nil
	case dsl.OpIn:
		if len(e.Value.List) == 0 {
			return "", nil, errors.New("in requires at least one value")
		}
		placeholders := make([]string, len(e.Value.List))
		args := make([]any, len(e.Value.List))
		for i := range e.Value.List {
			placeholders[i] = "?"
			args[i] = blobArg(valueToAny(&e.Value.List[i]))
		}
		return fmt.Sprintf("%s IN (%s)", col, strings.Join(placeholders, ", ")), args, nil
	default:
		return "", nil, fmt.Errorf("unsupported operator: %s", e.Op)
	}
}

// escapeLike escapes the LIKE metacharacters `%`, `_`, and the escape
// character itself so a `contains` filter always matches its input literally
// (e.g. searching for "100%" does not match "100X").
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "%", "\\%")
	s = strings.ReplaceAll(s, "_", "\\_")
	return s
}

func (c *exprCompiler) compileLogical(e *dsl.LogicalExpr) (sql string, args []any, err error) {
	leftSQL, leftArgs, err := c.compileExpr(e.Left)
	if err != nil {
		return "", nil, err
	}
	rightSQL, rightArgs, err := c.compileExpr(e.Right)
	if err != nil {
		return "", nil, err
	}

	logicalOp := "AND"
	if e.LogicalOp == dsl.OpOr {
		logicalOp = "OR"
	}

	sql = fmt.Sprintf("(%s %s %s)", leftSQL, logicalOp, rightSQL)
	args = make([]any, 0, len(leftArgs)+len(rightArgs))
	args = append(args, leftArgs...)
	args = append(args, rightArgs...)
	return sql, args, nil
}

// valueToAny converts a DSL Value to a Go value suitable for SQL arguments.
func valueToAny(v *dsl.Value) any {
	switch v.Type {
	case dsl.ValueString:
		return v.String
	case dsl.ValueInt:
		return v.Int
	case dsl.ValueFloat:
		return v.Float
	case dsl.ValueBool:
		if v.Bool {
			return int64(1)
		}
		return int64(0)
	default:
		return v.String
	}
}
