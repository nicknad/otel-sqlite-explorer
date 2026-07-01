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
// For the flat `logs` table the mapping is direct.
var fieldMap = map[string]string{
	"id":           "id",
	"timestamp":    "timestamp",
	"severity":     "severity",
	"service_name": "service_name",
	"trace_id":     "trace_id",
	"span_id":      "span_id",
	"body":         "body",
}

// Constant SQL fragments.
const selectAll = "id, timestamp, severity, service_name, trace_id, span_id, body"
const fromClause = "FROM logs"

// CompileGetByID builds a parameterized SELECT for a single log row by id.
func CompileGetByID(id int64) *CompiledQuery {
	return &CompiledQuery{
		SQL:  "SELECT " + selectAll + " " + fromClause + " WHERE id = ? LIMIT 1",
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

// Compile translates a validated, normalized DSL Query into a parameterized
// SQLite SELECT statement.
// The output is deterministic: same input → same SQL and args order.
func Compile(q *dsl.Query) (*CompiledQuery, error) {
	var b strings.Builder
	var args []any

	// ---- SELECT ----
	b.WriteString("SELECT ")
	b.WriteString(selectAll)

	// ---- FROM ----
	b.WriteString(" ")
	b.WriteString(fromClause)

	// ---- WHERE ----
	if q.Where != nil {
		whereSQL, whereArgs, err := compileExpr(q.Where)
		if err != nil {
			return nil, fmt.Errorf("compile where: %w", err)
		}
		b.WriteString(" WHERE ")
		b.WriteString(whereSQL)
		args = append(args, whereArgs...)
	}

	// ---- ORDER BY ----
	if len(q.Sort) > 0 {
		b.WriteString(" ORDER BY ")
		parts := make([]string, 0, len(q.Sort))
		for _, s := range q.Sort {
			col, ok := fieldMap[s.Field]
			if !ok {
				return nil, fmt.Errorf("unsupported sort field: %s", s.Field)
			}
			dir := "ASC"
			if s.Desc {
				dir = "DESC"
			}
			parts = append(parts, fmt.Sprintf("%s %s", col, dir))
		}
		b.WriteString(strings.Join(parts, ", "))
	}

	// ---- LIMIT / OFFSET ----
	b.WriteString(" LIMIT ? OFFSET ?")
	args = append(args, q.Limit, q.Offset)

	return &CompiledQuery{SQL: b.String(), Args: args}, nil
}

// compileExpr walks an expression tree and returns SQL + args.
func compileExpr(e dsl.Expr) (sql string, args []any, err error) {
	switch expr := e.(type) {
	case dsl.BinaryExpr:
		return compileBinary(&expr)
	case dsl.LogicalExpr:
		return compileLogical(&expr)
	default:
		return "", nil, fmt.Errorf("unsupported expression type %T", e)
	}
}

func compileBinary(e *dsl.BinaryExpr) (sql string, args []any, err error) {
	col, ok := fieldMap[e.Field]
	if !ok {
		return "", nil, fmt.Errorf("unsupported field in expression: %s", e.Field)
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

func compileLogical(e *dsl.LogicalExpr) (sql string, args []any, err error) {
	leftSQL, leftArgs, err := compileExpr(e.Left)
	if err != nil {
		return "", nil, err
	}
	rightSQL, rightArgs, err := compileExpr(e.Right)
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
