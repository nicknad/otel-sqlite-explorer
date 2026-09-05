package dsl

import (
	"errors"
	"fmt"
	"strings"
)

const (
	MaxLimit     = 1000
	MaxExprDepth = 10
	DefaultLimit = 100
	// MaxInValues bounds IN lists so a single query cannot exceed SQLite's
	// variable limit or blow up planning time.
	MaxInValues = 100
)

// Validate checks a Query against all business rules.
// Returns an error describing the first violation.
func Validate(q *Query) error {
	// --- Select ---
	sel := strings.TrimSpace(q.Select)
	if sel != "" && sel != "*" {
		for f := range strings.SplitSeq(sel, ",") {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			if !AllowedFields[f] {
				return fmt.Errorf("select: unknown field %q "+
					"(allowed: id, timestamp, severity, severity_number, service_name, trace_id, span_id, body, attributes_json)", f)
			}
		}
	}

	// --- Where ---
	if q.Where != nil {
		if err := validateExpr(q.Where); err != nil {
			return fmt.Errorf("where: %w", err)
		}
	}

	// --- Expression depth ---
	if q.Where != nil {
		d := exprDepth(q.Where)
		if d > MaxExprDepth {
			return fmt.Errorf("expression depth %d exceeds maximum of %d", d, MaxExprDepth)
		}
	}

	// --- Since ---
	if strings.TrimSpace(q.Since) != "" {
		if _, err := ParseSince(strings.TrimSpace(q.Since)); err != nil {
			return fmt.Errorf("since: %w", err)
		}
	}

	// --- Sort ---
	for i, s := range q.Sort {
		if !AllowedFields[s.Field] {
			return fmt.Errorf("sort[%d]: unknown field %q", i, s.Field)
		}
	}

	// --- Limit ---
	if q.Limit < 0 {
		return fmt.Errorf("limit must be non-negative, got %d", q.Limit)
	}
	if q.Limit > MaxLimit {
		return fmt.Errorf("limit %d exceeds maximum of %d", q.Limit, MaxLimit)
	}

	// --- Offset ---
	if q.Offset < 0 {
		return fmt.Errorf("offset must be non-negative, got %d", q.Offset)
	}

	return nil
}

func validateExpr(e Expr) error {
	switch expr := e.(type) {
	case BinaryExpr:
		if !AllowedFields[expr.Field] {
			return fmt.Errorf("unknown field %q", expr.Field)
		}
		if !AllowedOps[expr.Op] {
			return fmt.Errorf("unknown operator %q", expr.Op)
		}
		return validateValue(expr.Op, &expr.Value)

	case LogicalExpr:
		if !AllowedLogicalOps[expr.LogicalOp] {
			return fmt.Errorf("unknown logical operator %q", expr.LogicalOp)
		}
		if err := validateExpr(expr.Left); err != nil {
			return err
		}
		if err := validateExpr(expr.Right); err != nil {
			return err
		}
		return nil

	case MatchExpr:
		if strings.TrimSpace(expr.Query) == "" {
			return errors.New("match: query must be non-empty")
		}
		if len(expr.Query) > MatchMaxLen {
			return fmt.Errorf("match: query length %d exceeds maximum of %d", len(expr.Query), MatchMaxLen)
		}
		return nil

	default:
		return fmt.Errorf("unsupported expression type %T", e)
	}
}

// exprDepth returns the maximum nesting depth of an expression tree.
func exprDepth(e Expr) int {
	switch expr := e.(type) {
	case BinaryExpr:
		return 1
	case MatchExpr:
		return 1
	case LogicalExpr:
		left := exprDepth(expr.Left)
		right := exprDepth(expr.Right)
		if left > right {
			return 1 + left
		}
		return 1 + right
	default:
		return 0
	}
}

func validateValue(op Op, v *Value) error {
	switch op {
	case OpContains:
		if v.Type != ValueString {
			return fmt.Errorf("contains requires a string value, got %s", v.Type)
		}
	case OpBetween:
		if v.Min == nil || v.Max == nil {
			return errors.New("between requires min and max")
		}
		if v.Min.Type != v.Max.Type {
			return fmt.Errorf("between min/max type mismatch: %s vs %s", v.Min.Type, v.Max.Type)
		}
	case OpIn:
		if len(v.List) == 0 {
			return errors.New("in requires at least one value")
		}
		if len(v.List) > MaxInValues {
			return fmt.Errorf("in requires at most %d values, got %d", MaxInValues, len(v.List))
		}
		// all list elements must have the same type
		first := v.List[0].Type
		for i, item := range v.List {
			if item.Type == "" {
				return fmt.Errorf("in[%d]: value type is empty", i)
			}
			if item.Type != first {
				return fmt.Errorf("in[%d]: type mismatch: %s vs %s", i, item.Type, first)
			}
		}
	default:
		if v.Type == "" {
			return errors.New("value type is required")
		}
	}
	return nil
}
