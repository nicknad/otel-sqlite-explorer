// Package compiler translates DSL Query objects into parameterized SQLite
// statements.  This is the ONLY package that constructs SQL strings — no other
// package imports database/sql or builds SQL directly.
package compiler

import (
	"fmt"
	"strings"

	"log-explorer/internal/dsl"
)

// CompiledQuery holds a safe, parameterized SQL string and its arguments.
type CompiledQuery struct {
	SQL  string
	Args []any
}

// fieldMap translates DSL field names to SQL column expressions.
// The values match the column names exposed by the `logs` view in the
// otel-sqlite schema. BLOB columns (trace_id, span_id) are wrapped with
// hex() at the expression level — see colExpr and selectCols.
var fieldMap = map[string]string{
	"id":           "id",
	"timestamp":    "timestamp_ns",
	"severity":     "severity_text",
	"service_name": "service_name",
	"trace_id":     "trace_id",
	"span_id":      "span_id",
	"body":         "body",
}

// colExpr returns the SQL column expression for a DSL field, optionally
// qualified with the logs table alias for FTS5 JOIN disambiguation.
// BLOB columns (trace_id, span_id) are wrapped with hex() so string
// comparisons and scanning into Go strings work correctly.
func colExpr(field string, qualify bool) (string, error) {
	col, ok := fieldMap[field]
	if !ok {
		return "", fmt.Errorf("unsupported field: %s", field)
	}
	if qualify {
		col = "logs." + col
	}
	switch field {
	case "trace_id", "span_id":
		col = "hex(" + col + ")"
	}
	return col, nil
}

// logColumns defines the column names from the `logs` view, in LogRow
// scan order. These are used to build the SELECT list; BLOB columns
// (trace_id, span_id) are wrapped with hex() by selectCols.
var logColumns = []string{
	"id", "timestamp_ns", "severity_text", "service_name", "trace_id", "span_id", "body",
}

// Constant SQL fragments.
const fromLogs = "FROM logs"

// CompileGetByID builds a parameterized SELECT for a single log row by id.
func CompileGetByID(id int64) *CompiledQuery {
	return &CompiledQuery{
		SQL:  "SELECT " + selectCols(false) + " " + fromLogs + " WHERE id = ? LIMIT 1",
		Args: []any{id},
	}
}

// CompileGetAttrs builds a parameterized SELECT for the attributes of a log event.
func CompileGetAttrs(eventID int64) *CompiledQuery {
	return &CompiledQuery{
		SQL:  "SELECT key, value_type, string_value, int_value, double_value, bool_value, bytes_value FROM log_attr WHERE event_id = ? ORDER BY id",
		Args: []any{eventID},
	}
}

// selectCols returns the comma-separated column list, optionally qualified
// with the `logs.` table alias. Qualification is required when the FTS5
// index is joined in, because logs_fts also exposes body/service_name.
// BLOB columns (trace_id, span_id) are wrapped with hex() so they scan
// as hex-encoded text strings in Go.
func selectCols(qualify bool) string {
	parts := make([]string, len(logColumns))
	for i, c := range logColumns {
		expr := c
		if qualify {
			expr = "logs." + expr
		}
		switch c {
		case "trace_id", "span_id":
			expr = "hex(" + expr + ")"
		}
		parts[i] = expr
	}
	return strings.Join(parts, ", ")
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
	b.WriteString("SELECT ")
	b.WriteString(selectCols(matched))

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
	}

	// ---- LIMIT / OFFSET ----
	b.WriteString(" LIMIT ? OFFSET ?")
	args = append(args, q.Limit, q.Offset)

	return &CompiledQuery{SQL: b.String(), Args: args}, nil
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

	switch e.Op {
	case dsl.OpEq:
		return fmt.Sprintf("%s = ?", col), []any{valueToAny(&e.Value)}, nil
	case dsl.OpNe:
		return fmt.Sprintf("%s != ?", col), []any{valueToAny(&e.Value)}, nil
	case dsl.OpGt:
		return fmt.Sprintf("%s > ?", col), []any{valueToAny(&e.Value)}, nil
	case dsl.OpGte:
		return fmt.Sprintf("%s >= ?", col), []any{valueToAny(&e.Value)}, nil
	case dsl.OpLt:
		return fmt.Sprintf("%s < ?", col), []any{valueToAny(&e.Value)}, nil
	case dsl.OpLte:
		return fmt.Sprintf("%s <= ?", col), []any{valueToAny(&e.Value)}, nil
	case dsl.OpContains:
		val := valueToAny(&e.Value)
		str, ok := val.(string)
		if !ok {
			return "", nil, fmt.Errorf("contains requires a string value")
		}
		return fmt.Sprintf("%s LIKE ?", col), []any{"%" + str + "%"}, nil
	case dsl.OpBetween:
		if e.Value.Min == nil || e.Value.Max == nil {
			return "", nil, fmt.Errorf("between requires min and max")
		}
		return fmt.Sprintf("%s BETWEEN ? AND ?", col),
			[]any{valueToAny(e.Value.Min), valueToAny(e.Value.Max)}, nil
	case dsl.OpIn:
		if len(e.Value.List) == 0 {
			return "", nil, fmt.Errorf("in requires at least one value")
		}
		placeholders := make([]string, len(e.Value.List))
		args := make([]any, len(e.Value.List))
		for i := range e.Value.List {
			placeholders[i] = "?"
			args[i] = valueToAny(&e.Value.List[i])
		}
		return fmt.Sprintf("%s IN (%s)", col, strings.Join(placeholders, ", ")), args, nil
	default:
		return "", nil, fmt.Errorf("unsupported operator: %s", e.Op)
	}
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
	if e.Op == dsl.OpOr {
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
