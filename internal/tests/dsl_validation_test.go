package tests

import (
	"encoding/json"
	"strings"
	"testing"

	"log-explorer/internal/dsl"
)

func TestValidQueryPassesValidation(t *testing.T) {
	q := dsl.Query{
		Select: "*",
		Limit:  100,
	}
	if err := dsl.Validate(&q); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestInvalidFieldIsRejected(t *testing.T) {
	q := dsl.Query{
		Where: dsl.BinaryExpr{
			Op:    dsl.OpEq,
			Field: "nonexistent_field",
			Value: dsl.Value{Type: dsl.ValueString, String: "x"},
		},
		Limit: 100,
	}
	if err := dsl.Validate(&q); err == nil {
		t.Fatal("expected error for invalid field, got nil")
	}
}

func TestInvalidOperatorIsRejected(t *testing.T) {
	q := dsl.Query{
		Where: dsl.BinaryExpr{
			Op:    dsl.Op("regexp"),
			Field: "body",
			Value: dsl.Value{Type: dsl.ValueString, String: "foo"},
		},
		Limit: 100,
	}
	if err := dsl.Validate(&q); err == nil {
		t.Fatal("expected error for invalid operator, got nil")
	}
}

func TestLimitExceedsMaxIsRejected(t *testing.T) {
	q := dsl.Query{
		Limit: 1001,
	}
	if err := dsl.Validate(&q); err == nil {
		t.Fatal("expected error for limit > 1000, got nil")
	}
}

func TestMaxLimitIsAllowed(t *testing.T) {
	q := dsl.Query{
		Limit: 1000,
	}
	if err := dsl.Validate(&q); err != nil {
		t.Fatalf("expected no error for limit=1000, got: %v", err)
	}
}

func TestDeeplyNestedExpressionIsRejected(t *testing.T) {
	// Build a chain of AND nodes with depth 11 (max allowed is 10).
	leaf := dsl.BinaryExpr{
		Op:    dsl.OpEq,
		Field: "severity",
		Value: dsl.Value{Type: dsl.ValueString, String: "ERROR"},
	}
	// Nest it 10 levels deep → depth = 11 (> MaxExprDepth=10)
	current := dsl.Expr(leaf)
	for range 10 {
		current = dsl.LogicalExpr{
			LogicalOp: dsl.OpAnd,
			Left:      current,
			Right:     leaf,
		}
	}

	q := dsl.Query{Where: current, Limit: 100}
	if err := dsl.Validate(&q); err == nil {
		t.Fatal("expected error for deeply nested expression, got nil")
	}
}

func TestEmptyWhereIsAllowed(t *testing.T) {
	q := dsl.Query{
		Limit: 100,
	}
	if err := dsl.Validate(&q); err != nil {
		t.Fatalf("expected no error for empty where, got: %v", err)
	}
}

func TestSelectUnknownFieldIsRejected(t *testing.T) {
	q := dsl.Query{
		Select: "timestamp, bad_field",
		Limit:  100,
	}
	if err := dsl.Validate(&q); err == nil {
		t.Fatal("expected error for unknown select field, got nil")
	}
}

func TestMatchEmptyIsRejected(t *testing.T) {
	q := dsl.Query{
		Where: dsl.MatchExpr{Query: "   "},
		Limit: 100,
	}
	if err := dsl.Validate(&q); err == nil {
		t.Fatal("expected error for empty match query, got nil")
	}
}

func TestMatchTooLongIsRejected(t *testing.T) {
	q := dsl.Query{
		Where: dsl.MatchExpr{Query: strings.Repeat("a", dsl.MatchMaxLen+1)},
		Limit: 100,
	}
	if err := dsl.Validate(&q); err == nil {
		t.Fatal("expected error for over-long match query, got nil")
	}
}

func TestMatchValidIsAllowed(t *testing.T) {
	q := dsl.Query{
		Where: dsl.MatchExpr{Query: "timeout gateway"},
		Limit: 100,
	}
	if err := dsl.Validate(&q); err != nil {
		t.Fatalf("expected no error for valid match, got: %v", err)
	}
}

func TestMatchInLogicalTreeIsAllowed(t *testing.T) {
	q := dsl.Query{
		Where: dsl.LogicalExpr{
			LogicalOp: dsl.OpAnd,
			Left:      dsl.BinaryExpr{Op: dsl.OpEq, Field: "severity", Value: dsl.Value{Type: dsl.ValueString, String: "ERROR"}},
			Right:     dsl.MatchExpr{Query: "timeout"},
		},
		Limit: 100,
	}
	if err := dsl.Validate(&q); err != nil {
		t.Fatalf("expected no error for match in logical tree, got: %v", err)
	}
}

func TestSinceDayAndWeekShorthandsAreAllowed(t *testing.T) {
	for _, since := range []string{"1h", "24h", "7d", "1w", "30d"} {
		q := dsl.Query{Since: since, Limit: 10}
		if err := dsl.Validate(&q); err != nil {
			t.Errorf("since=%q: expected no error, got: %v", since, err)
			continue
		}
		dsl.Normalize(&q)
		if q.Since != "" {
			t.Errorf("since=%q: expected Since to be consumed, got %q", since, q.Since)
		}
		if q.Where == nil {
			t.Errorf("since=%q: expected a timestamp filter to be added", since)
		}
	}
}

func TestSinceInvalidIsRejected(t *testing.T) {
	for _, since := range []string{"bogus", "7x", "-1h", "0s", "tomorrow", "d", "w"} {
		q := dsl.Query{Since: since, Limit: 10}
		if err := dsl.Validate(&q); err == nil {
			t.Errorf("since=%q: expected error, got nil", since)
		}
	}
}

func TestUnknownTopLevelFieldIsRejected(t *testing.T) {
	var q dsl.Query
	if err := json.Unmarshal([]byte(`{"limt": 10, "limit": 10}`), &q); err == nil {
		t.Fatal("expected error for unknown top-level field, got nil")
	}
}

func TestNumericShorthandComparison(t *testing.T) {
	// JSON numbers (not strings) must work as comparison values.
	var q dsl.Query
	if err := json.Unmarshal([]byte(`{"where": {"eq": ["severity_number", 17]}, "limit": 10}`), &q); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	be, ok := q.Where.(dsl.BinaryExpr)
	if !ok {
		t.Fatalf("expected BinaryExpr, got %T", q.Where)
	}
	if be.Value.Type != dsl.ValueInt || be.Value.Int != 17 {
		t.Errorf("expected int 17, got %+v", be.Value)
	}
	if err := dsl.Validate(&q); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestLargeTimestampKeepsPrecision(t *testing.T) {
	// Nanosecond timestamps exceed float64's exact-integer range; they must
	// survive the JSON round-trip without precision loss.
	var q dsl.Query
	if err := json.Unmarshal([]byte(
		`{"where": {"between": ["timestamp", 1700000000000000000, 1700000099000000000]}, "limit": 10}`,
	), &q); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	be, ok := q.Where.(dsl.BinaryExpr)
	if !ok {
		t.Fatalf("expected BinaryExpr, got %T", q.Where)
	}
	if be.Value.Min.Int != 1700000000000000000 || be.Value.Max.Int != 1700000099000000000 {
		t.Errorf("precision lost: min=%d max=%d", be.Value.Min.Int, be.Value.Max.Int)
	}
}

func TestNumericStringStaysString(t *testing.T) {
	// A fully-numeric shorthand string must not be truncated into a prefix
	// number ("123abc" is a string, not 123).
	var q dsl.Query
	if err := json.Unmarshal([]byte(`{"where": {"eq": ["body", "123abc"]}, "limit": 10}`), &q); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	be := q.Where.(dsl.BinaryExpr)
	if be.Value.Type != dsl.ValueString || be.Value.String != "123abc" {
		t.Errorf("expected string %q, got %+v", "123abc", be.Value)
	}
}

func TestNumericStringContainsStaysString(t *testing.T) {
	// Searching a body for "404" must remain a substring match.
	var q dsl.Query
	if err := json.Unmarshal([]byte(`{"where": {"contains": ["body", "404"]}, "limit": 10}`), &q); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	be := q.Where.(dsl.BinaryExpr)
	if be.Value.Type != dsl.ValueString || be.Value.String != "404" {
		t.Errorf("expected string %q, got %+v", "404", be.Value)
	}
	if err := dsl.Validate(&q); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestNaryAndOrAccepted(t *testing.T) {
	var q dsl.Query
	if err := json.Unmarshal([]byte(`{"where": {"and": [
		{"eq": ["severity", "ERROR"]},
		{"eq": ["service_name", "api-gateway"]},
		{"contains": ["body", "timeout"]}
	]}, "limit": 10}`), &q); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := dsl.Validate(&q); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestSingleChildAndIsRejected(t *testing.T) {
	var q dsl.Query
	if err := json.Unmarshal([]byte(`{"where": {"and": [{"eq": ["severity", "ERROR"]}]}, "limit": 10}`), &q); err == nil {
		t.Fatal("expected error for single-child and, got nil")
	}
}

func TestContainsNonStringIsRejected(t *testing.T) {
	q := dsl.Query{
		Where: dsl.BinaryExpr{
			Op:    dsl.OpContains,
			Field: "body",
			Value: dsl.Value{Type: dsl.ValueInt, Int: 404},
		},
		Limit: 10,
	}
	if err := dsl.Validate(&q); err == nil {
		t.Fatal("expected error for non-string contains value, got nil")
	}
}

func TestInListTooLongIsRejected(t *testing.T) {
	vals := make([]dsl.Value, dsl.MaxInValues+1)
	for i := range vals {
		vals[i] = dsl.Value{Type: dsl.ValueString, String: "x"}
	}
	q := dsl.Query{
		Where: dsl.BinaryExpr{Op: dsl.OpIn, Field: "service_name", Value: dsl.Value{List: vals}},
		Limit: 10,
	}
	if err := dsl.Validate(&q); err == nil {
		t.Fatal("expected error for over-long in list, got nil")
	}
}

func TestInListTypeMismatchIsRejected(t *testing.T) {
	q := dsl.Query{
		Where: dsl.BinaryExpr{
			Op:    dsl.OpIn,
			Field: "service_name",
			Value: dsl.Value{List: []dsl.Value{
				{Type: dsl.ValueString, String: "a"},
				{Type: dsl.ValueInt, Int: 1},
			}},
		},
		Limit: 10,
	}
	if err := dsl.Validate(&q); err == nil {
		t.Fatal("expected error for mixed-type in list, got nil")
	}
}
