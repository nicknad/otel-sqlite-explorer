package tests

import (
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
	for i := 0; i < 10; i++ {
		current = dsl.LogicalExpr{
			Op:    dsl.OpAnd,
			Left:  current,
			Right: leaf,
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
			Op:    dsl.OpAnd,
			Left:  dsl.BinaryExpr{Op: dsl.OpEq, Field: "severity", Value: dsl.Value{Type: dsl.ValueString, String: "ERROR"}},
			Right: dsl.MatchExpr{Query: "timeout"},
		},
		Limit: 100,
	}
	if err := dsl.Validate(&q); err != nil {
		t.Fatalf("expected no error for match in logical tree, got: %v", err)
	}
}
