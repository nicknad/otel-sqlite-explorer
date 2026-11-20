package query

import (
	"strconv"
	"strings"

	"log-explorer/internal/dsl"
	"log-explorer/internal/severity"
)

// FormFilters carries the raw, unvalidated filter values of the HTML form.
// Text filters arrive pre-trimmed from the form adapter; Since, Limit and
// Offset keep their raw form values (Limit/Offset rules live here, Since is
// consumed by Normalize after validation trims it).
type FormFilters struct {
	Service  string
	Severity string
	TraceID  string
	SpanID   string
	Body     string
	Search   string
	Since    string
	DateFrom string
	DateTo   string
	Limit    string
	Offset   string
}

// formQuery assembles the DSL query described by raw form values, plus the
// canonical echo the form re-renders (severity upper-cased; the effective
// limit/offset are filled in by the pipeline after Normalize).
func formQuery(f FormFilters) (*dsl.Query, Echo, error) {
	echo := Echo{Since: f.Since}

	var exprs []dsl.Expr
	if f.Service != "" {
		exprs = append(exprs, dsl.BinaryExpr{Op: dsl.OpContains, Field: "service_name", Value: dsl.Value{Type: dsl.ValueString, String: f.Service}})
	}
	if f.Severity != "" {
		n := severity.Number(f.Severity)
		if n == 0 {
			return nil, echo, validationf("severity", "unknown severity %q (expected DEBUG, INFO, WARN, ERROR, or FATAL)", f.Severity)
		}
		// Canonicalize so the form dropdown reflects the active filter even
		// when the user typed a lowercase severity in the URL.
		echo.Severity = strings.ToUpper(f.Severity)
		exprs = append(exprs, dsl.BinaryExpr{
			Op:    dsl.OpGte,
			Field: "severity_number",
			Value: dsl.Value{Type: dsl.ValueInt, Int: n},
		})
	}
	if f.TraceID != "" {
		exprs = append(exprs, dsl.BinaryExpr{Op: dsl.OpEq, Field: "trace_id", Value: dsl.Value{Type: dsl.ValueString, String: f.TraceID}})
	}
	if f.SpanID != "" {
		exprs = append(exprs, dsl.BinaryExpr{Op: dsl.OpEq, Field: "span_id", Value: dsl.Value{Type: dsl.ValueString, String: f.SpanID}})
	}
	if f.Body != "" {
		exprs = append(exprs, dsl.BinaryExpr{Op: dsl.OpContains, Field: "body", Value: dsl.Value{Type: dsl.ValueString, String: f.Body}})
	}
	// Date range filters — convert to timestamp nanoseconds for the DSL.
	// A date-only From starts at the beginning of that day; a date-only To
	// covers through the end of that day. Unparseable input is a client
	// error, not a silent no-op.
	if f.DateFrom != "" {
		ts, err := parseDateTime(f.DateFrom, false)
		if err != nil {
			return nil, echo, validationf("date_from", "invalid date_from: %v", err)
		}
		exprs = append(exprs, dsl.BinaryExpr{Op: dsl.OpGte, Field: "timestamp", Value: dsl.Value{Type: dsl.ValueInt, Int: ts}})
	}
	if f.DateTo != "" {
		ts, err := parseDateTime(f.DateTo, true)
		if err != nil {
			return nil, echo, validationf("date_to", "invalid date_to: %v", err)
		}
		exprs = append(exprs, dsl.BinaryExpr{Op: dsl.OpLte, Field: "timestamp", Value: dsl.Value{Type: dsl.ValueInt, Int: ts}})
	}
	if f.Search != "" {
		exprs = append(exprs, dsl.MatchExpr{Query: f.Search})
	}

	// Limit/offset keep the form's own rule: empty keeps the default (0
	// means default too), out-of-range values are rejected with the form's
	// legacy wording.
	limit := dsl.DefaultLimit
	if v := f.Limit; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > dsl.MaxLimit {
			return nil, echo, validationf("limit", "limit must be an integer between 1 and %d, or 0 for the default", dsl.MaxLimit)
		}
		if n > 0 {
			limit = n
		}
	}
	offset := 0
	if v := f.Offset; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, echo, validationf("offset", "offset must be a non-negative integer")
		}
		offset = n
	}

	q := &dsl.Query{
		Select: "*",
		Since:  f.Since,
		Limit:  limit,
		Offset: offset,
	}
	if len(exprs) > 0 {
		q.Where = andExprs(exprs)
	}
	return q, echo, nil
}

// andExprs combines a slice of expressions into a single AND tree.
// For 0 expressions returns nil; for 1 returns it directly.
func andExprs(exprs []dsl.Expr) dsl.Expr {
	if len(exprs) == 0 {
		return nil
	}
	result := exprs[0]
	for _, e := range exprs[1:] {
		result = dsl.LogicalExpr{LogicalOp: dsl.OpAnd, Left: result, Right: e}
	}
	return result
}
