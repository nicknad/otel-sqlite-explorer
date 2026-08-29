// Package dsl defines the strict Query DSL for the log-explorer service.
// This is the only way queries enter the system — raw SQL is never exposed.
package dsl

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ============================================================================
// Operator types
// ============================================================================

// Op represents a comparison operator in a binary expression.
type Op string

const (
	OpEq       Op = "eq"
	OpNe       Op = "ne"
	OpGt       Op = "gt"
	OpGte      Op = "gte"
	OpLt       Op = "lt"
	OpLte      Op = "lte"
	OpContains Op = "contains"
	OpBetween  Op = "between"
	OpIn       Op = "in"
	OpMatch    Op = "match" // FTS5 full-text match (leaf node)
)

// LogicalOp represents a logical combinator.
type LogicalOp string

const (
	OpAnd LogicalOp = "and"
	OpOr  LogicalOp = "or"
)

// ============================================================================
// Value types
// ============================================================================

// ValueType enumerates the supported scalar value types.
type ValueType string

const (
	ValueString ValueType = "string"
	ValueInt    ValueType = "int"
	ValueFloat  ValueType = "float"
	ValueBool   ValueType = "bool"
)

// Value holds a strongly-typed scalar or composite value for DSL expressions.
type Value struct {
	Type   ValueType `json:"type"`
	String string    `json:"string,omitempty"`
	Int    int64     `json:"int,omitempty"`
	Float  float64   `json:"float,omitempty"`
	Bool   bool      `json:"bool,omitempty"`
	List   []Value   `json:"list,omitempty"` // for IN
	Min    *Value    `json:"min,omitempty"`  // for BETWEEN
	Max    *Value    `json:"max,omitempty"`  // for BETWEEN
}

// ============================================================================
// Expression tree
// ============================================================================

// Expr is the common interface for all expression nodes.
type Expr interface {
	exprNode()
}

// BinaryExpr represents a comparison: field OP value.
type BinaryExpr struct {
	Op    Op     `json:"op"`
	Field string `json:"field"`
	Value Value  `json:"value"`
}

func (BinaryExpr) exprNode() {}

// LogicalExpr combines two sub-expressions with AND / OR.
type LogicalExpr struct {
	LogicalOp LogicalOp `json:"logical_op"`
	Left      Expr      `json:"left"`
	Right     Expr      `json:"right"`
}

func (LogicalExpr) exprNode() {}

// MatchExpr is a leaf node representing an FTS5 full-text search.
// It is only valid when the backing database has a logs_fts index; the
// compiler falls back to a body substring match otherwise.
type MatchExpr struct {
	Query string `json:"query"`
}

func (MatchExpr) exprNode() {}

// ============================================================================
// Sort and Query
// ============================================================================

// Sort specifies an ordering over one allowed field.
type Sort struct {
	Field string `json:"field"`
	Desc  bool   `json:"desc"`
}

// Query is the top-level DSL query object.
// Every user query is parsed into this structure and validated before compilation.
type Query struct {
	Select string `json:"select"`          // comma-separated field list or "*"
	Where  Expr   `json:"where,omitempty"` // optional filter tree
	Since  string `json:"since,omitempty"` // human duration e.g. "24h", "7d"
	Sort   []Sort `json:"sort,omitempty"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
}

// ============================================================================
// Whitelists (used by validator)
// ============================================================================

var AllowedFields = map[string]bool{
	"id":              true,
	"timestamp":       true,
	"severity":        true,
	"severity_number": true,
	"service_name":    true,
	"trace_id":        true,
	"span_id":         true,
	"body":            true,
}

var AllowedOps = map[Op]bool{
	OpEq:       true,
	OpNe:       true,
	OpGt:       true,
	OpGte:      true,
	OpLt:       true,
	OpLte:      true,
	OpContains: true,
	OpBetween:  true,
	OpIn:       true,
}

var AllowedLogicalOps = map[LogicalOp]bool{
	OpAnd: true,
	OpOr:  true,
}

// MatchMaxLen bounds the FTS5 query string length to prevent abuse.
const MatchMaxLen = 256

// HasMatch reports whether the expression tree contains a MatchExpr leaf.
// It is used by Normalize (to skip the default time sort in favour of
// relevance ranking) and by the compiler (to decide on a JOIN strategy).
func HasMatch(e Expr) bool {
	switch x := e.(type) {
	case MatchExpr:
		return true
	case LogicalExpr:
		return HasMatch(x.Left) || HasMatch(x.Right)
	}
	return false
}

// ============================================================================
// JSON unmarshalling — supports two wire formats:
//
// Shorthand (primary):
//
//	{"eq": ["field", "value"]}
//	{"and": [expr1, expr2]}
//	{"between": ["field", 10, 100]}
//	{"in": ["field", ["a","b"]]}
//
// Verbose (legacy):
//
//	{"type": "binary", "op": "eq", "field": "field", "value": {"type": "string", "string": "value"}}
//	{"type": "logical", "logical_op": "and", "exprs": [left, right]}
// ============================================================================

// exprJSON is the wire representation of an expression (verbose format).
type exprJSON struct {
	Type      string     `json:"type"`
	Op        Op         `json:"op,omitempty"`
	Field     string     `json:"field,omitempty"`
	Value     *Value     `json:"value,omitempty"`
	LogicalOp LogicalOp  `json:"logical_op,omitempty"`
	Exprs     []exprJSON `json:"exprs,omitempty"`
	Query     string     `json:"query,omitempty"` // for type "match"
}

// UnmarshalJSON implements json.Unmarshaler for Query.
// It tries the shorthand format first, then falls back to the verbose format.
func (q *Query) UnmarshalJSON(data []byte) error {
	// Try shorthand format first.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	// Decode scalar fields.
	if sel, ok := raw["select"]; ok {
		if err := json.Unmarshal(sel, &q.Select); err != nil {
			return fmt.Errorf("select: %w", err)
		}
	}
	if since, ok := raw["since"]; ok {
		if err := json.Unmarshal(since, &q.Since); err != nil {
			return fmt.Errorf("since: %w", err)
		}
	}
	if limit, ok := raw["limit"]; ok {
		if err := json.Unmarshal(limit, &q.Limit); err != nil {
			return fmt.Errorf("limit: %w", err)
		}
	}
	if offset, ok := raw["offset"]; ok {
		if err := json.Unmarshal(offset, &q.Offset); err != nil {
			return fmt.Errorf("offset: %w", err)
		}
	}
	if sortRaw, ok := raw["sort"]; ok {
		if err := json.Unmarshal(sortRaw, &q.Sort); err != nil {
			return fmt.Errorf("sort: %w", err)
		}
	}

	// Parse the where clause.
	if whereRaw, ok := raw["where"]; ok && len(whereRaw) > 0 {
		expr, err := unmarshalExpr(whereRaw)
		if err != nil {
			return fmt.Errorf("where: %w", err)
		}
		q.Where = expr
	}

	return nil
}

// unmarshalExpr parses an expression from raw JSON, detecting the format.
func unmarshalExpr(data json.RawMessage) (Expr, error) {
	// Try shorthand format first.
	expr, err := tryShorthandExpr(data)
	if err == nil {
		return expr, nil
	}

	// Fall back to verbose format.
	var ve exprJSON
	if err := json.Unmarshal(data, &ve); err != nil {
		return nil, fmt.Errorf("invalid expression: %s", string(data))
	}
	return exprFromVerbose(&ve)
}

// tryShorthandExpr attempts to parse an expression using the shorthand format.
// In shorthand, expr is a JSON object with exactly one key (the operator).
func tryShorthandExpr(data json.RawMessage) (Expr, error) {
	// Unmarshal into a map with one key.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if len(raw) != 1 {
		return nil, errors.New("shorthand requires exactly one key")
	}

	// Capture the single key for error reporting.
	var opKey string
	var opVal json.RawMessage
	for k, v := range raw {
		opKey = k
		opVal = v
	}

	// Check logical operators first.
	switch LogicalOp(opKey) {
	case OpAnd, OpOr:
		exprs, err := unmarshalExprSlice(opVal)
		if err != nil {
			return nil, err
		}
		if len(exprs) != 2 {
			return nil, fmt.Errorf("%s requires exactly 2 sub-expressions, got %d", opKey, len(exprs))
		}
		return LogicalExpr{LogicalOp: LogicalOp(opKey), Left: exprs[0], Right: exprs[1]}, nil
	}

	// match is a leaf with a single string value (FTS5 query syntax).
	if opKey == string(OpMatch) {
		var s string
		if err := json.Unmarshal(opVal, &s); err != nil {
			return nil, errors.New("match requires a string value")
		}
		return MatchExpr{Query: s}, nil
	}

	// Check comparison operators.
	switch Op(opKey) {
	case OpEq, OpNe, OpGt, OpGte, OpLt, OpLte, OpContains:
		parts, err := unmarshalStringSlice(opVal)
		if err != nil {
			return nil, err
		}
		if len(parts) != 2 {
			return nil, fmt.Errorf("%s requires [field, value], got %d elements", opKey, len(parts))
		}
		return BinaryExpr{Op: Op(opKey), Field: parts[0], Value: stringToValue(parts[1])}, nil

	case OpBetween:
		arr, err := unmarshalMixedSlice(opVal)
		if err != nil {
			return nil, err
		}
		if len(arr) != 3 {
			return nil, fmt.Errorf("between requires [field, min, max], got %d elements", len(arr))
		}
		field, ok := arr[0].(string)
		if !ok {
			return nil, errors.New("between: first element must be a field name")
		}
		minVal, minErr := jsonToValue(arr[1])
		maxVal, maxErr := jsonToValue(arr[2])
		if minErr != nil || maxErr != nil {
			return nil, errors.New("between: invalid min/max values")
		}
		return BinaryExpr{
			Op:    OpBetween,
			Field: field,
			Value: Value{Min: &minVal, Max: &maxVal},
		}, nil

	case OpIn:
		arr, err := unmarshalMixedSlice(opVal)
		if err != nil {
			return nil, err
		}
		if len(arr) != 2 {
			return nil, fmt.Errorf("in requires [field, values], got %d elements", len(arr))
		}
		field, ok := arr[0].(string)
		if !ok {
			return nil, errors.New("in: first element must be a field name")
		}
		list, ok := arr[1].([]any)
		if !ok {
			return nil, errors.New("in: second element must be an array")
		}
		vals := make([]Value, len(list))
		for i, item := range list {
			v, err := jsonToValue(item)
			if err != nil {
				return nil, fmt.Errorf("in[%d]: %w", i, err)
			}
			vals[i] = v
		}
		return BinaryExpr{Op: OpIn, Field: field, Value: Value{List: vals}}, nil
	}

	return nil, fmt.Errorf("unknown operator: %s", opKey)
}

// unmarshalExprSlice parses a JSON array of expressions.
func unmarshalExprSlice(data json.RawMessage) ([]Expr, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	exprs := make([]Expr, len(raw))
	for i, r := range raw {
		e, err := unmarshalExpr(r)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		exprs[i] = e
	}
	return exprs, nil
}

// unmarshalStringSlice parses a JSON array of strings.
func unmarshalStringSlice(data json.RawMessage) ([]string, error) {
	var raw []string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// unmarshalMixedSlice parses a JSON array of mixed types (used for between/in).
func unmarshalMixedSlice(data json.RawMessage) ([]any, error) {
	var raw []any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// stringToValue creates a Value from a JSON string, trying to parse numbers.
func stringToValue(s string) Value {
	// If it looks like a number, parse as int.
	if s != "" && s[0] >= '0' && s[0] <= '9' {
		// Try int
		var i int64
		if _, err := fmt.Sscanf(s, "%d", &i); err == nil {
			return Value{Type: ValueInt, Int: i}
		}
		// Try float
		var f float64
		if _, err := fmt.Sscanf(s, "%f", &f); err == nil {
			return Value{Type: ValueFloat, Float: f}
		}
	}
	return Value{Type: ValueString, String: s}
}

// jsonToValue converts a parsed JSON value to a DSL Value with inferred type.
func jsonToValue(v any) (Value, error) {
	switch val := v.(type) {
	case string:
		return Value{Type: ValueString, String: val}, nil
	case float64:
		// JSON numbers decode as float64 by default.
		// Check if it's a whole number.
		if val == float64(int64(val)) {
			return Value{Type: ValueInt, Int: int64(val)}, nil
		}
		return Value{Type: ValueFloat, Float: val}, nil
	case bool:
		return Value{Type: ValueBool, Bool: val}, nil
	case json.Number:
		if i, err := val.Int64(); err == nil {
			return Value{Type: ValueInt, Int: i}, nil
		}
		f, err := val.Float64()
		if err != nil {
			return Value{}, fmt.Errorf("cannot parse number: %s", val)
		}
		return Value{Type: ValueFloat, Float: f}, nil
	default:
		return Value{}, fmt.Errorf("unsupported JSON value type: %T", v)
	}
}

// exprFromVerbose converts a verbose-format exprJSON into an Expr.
func exprFromVerbose(j *exprJSON) (Expr, error) {
	switch j.Type {
	case "binary":
		if j.Value == nil {
			return nil, errors.New("binary expression missing value")
		}
		return BinaryExpr{Op: j.Op, Field: j.Field, Value: *j.Value}, nil
	case "logical":
		if len(j.Exprs) != 2 {
			return nil, errors.New("logical expression requires exactly 2 sub-expressions")
		}
		left, err := exprFromVerbose(&j.Exprs[0])
		if err != nil {
			return nil, err
		}
		right, err := exprFromVerbose(&j.Exprs[1])
		if err != nil {
			return nil, err
		}
		return LogicalExpr{LogicalOp: j.LogicalOp, Left: left, Right: right}, nil
	case "match":
		return MatchExpr{Query: j.Query}, nil
	default:
		return nil, fmt.Errorf("unknown expression type %q", j.Type)
	}
}
