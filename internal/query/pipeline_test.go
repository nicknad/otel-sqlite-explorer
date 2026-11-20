package query

import (
	"context"
	"errors"
	"strings"
	"testing"

	"log-explorer/internal/compiler"
	"log-explorer/internal/db"
	"log-explorer/internal/dsl"
)

// fakeExec is the pipeline's second executor adapter: it records compiled
// queries and answers from scripted state. It encodes the executor's
// interface contract only — SQL reality is asserted by the fixture-backed
// integration tests, never here.
type fakeExec struct {
	hasFTS      bool
	rows        []db.LogRow
	executeErr  error
	validateErr error
	queries     []*compiler.CompiledQuery
	validated   []string
}

func (f *fakeExec) ExecuteContext(_ context.Context, cq *compiler.CompiledQuery) ([]db.LogRow, error) {
	f.queries = append(f.queries, cq)
	if f.executeErr != nil {
		return nil, f.executeErr
	}
	return f.rows, nil
}

func (f *fakeExec) HasFTS() bool { return f.hasFTS }

func (f *fakeExec) ValidateMatch(_ context.Context, q string) error {
	f.validated = append(f.validated, q)
	return f.validateErr
}

func rows(n int) []db.LogRow {
	out := make([]db.LogRow, n)
	for i := range out {
		out[i] = db.LogRow{ID: int64(i + 1), Body: "row"}
	}
	return out
}

func TestFormFallsBackToSubstringWithoutFTS(t *testing.T) {
	exec := &fakeExec{rows: rows(1)}
	page, err := New(exec).PageFromForm(context.Background(), FormFilters{Search: "needle"})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if len(exec.queries) != 1 {
		t.Fatalf("expected exactly one compiled query, got %d", len(exec.queries))
	}
	sql := exec.queries[0].SQL
	if strings.Contains(sql, "logs_fts") {
		t.Errorf("fallback must not touch the FTS index, SQL: %s", sql)
	}
	if !strings.Contains(sql, "LIKE") {
		t.Errorf("fallback must compile to a substring match, SQL: %s", sql)
	}
	if len(exec.validated) != 0 {
		t.Errorf("match syntax must not be validated without an index, validated: %v", exec.validated)
	}
	if page.HasNext || page.HasPrev {
		t.Errorf("unexpected pagination flags: %+v", page)
	}
}

func TestFormKeepsIndexSearchWithFTS(t *testing.T) {
	exec := &fakeExec{hasFTS: true, rows: rows(1)}
	if _, err := New(exec).PageFromForm(context.Background(), FormFilters{Search: "needle"}); err != nil {
		t.Fatalf("page: %v", err)
	}
	sql := exec.queries[0].SQL
	if !strings.Contains(sql, "logs_fts MATCH") {
		t.Errorf("index search must compile to MATCH, SQL: %s", sql)
	}
	if !strings.Contains(sql, "JOIN logs_fts") {
		t.Errorf("index search must join the index, SQL: %s", sql)
	}
	if !strings.Contains(sql, "bm25") {
		t.Errorf("match without explicit sort must rank by bm25, SQL: %s", sql)
	}
	if len(exec.validated) != 1 || exec.validated[0] != "needle" {
		t.Errorf("match syntax must be validated against the index, validated: %v", exec.validated)
	}
}

func TestPageDetectsNextPageAtMaxLimit(t *testing.T) {
	// Regression: re-normalizing the limit+1 probe clamped it back to
	// MaxLimit, so the Next button was always disabled at limit=1000 even
	// with more rows available.
	exec := &fakeExec{rows: rows(dsl.MaxLimit + 1)}
	page, err := New(exec).PageFromForm(context.Background(), FormFilters{Limit: "1000"})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if !page.HasNext {
		t.Error("expected HasNext=true when more rows exist than MaxLimit")
	}
	if len(page.Rows) != dsl.MaxLimit {
		t.Errorf("expected %d rows, got %d", dsl.MaxLimit, len(page.Rows))
	}
	if page.HasPrev {
		t.Error("expected HasPrev=false on the first page")
	}
	last := exec.queries[len(exec.queries)-1]
	if got := last.Args[len(last.Args)-2]; got != dsl.MaxLimit+1 {
		t.Errorf("probe must fetch MaxLimit+1 rows, got %v", got)
	}
}

func TestPageProbeHeadroomAtSmallLimit(t *testing.T) {
	exec := &fakeExec{rows: rows(11)}
	page, err := New(exec).PageFromForm(context.Background(), FormFilters{Limit: "10"})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if !page.HasNext || len(page.Rows) != 10 || page.NextOffset != 10 {
		t.Errorf("unexpected page: rows=%d next=%v nextOffset=%d", len(page.Rows), page.HasNext, page.NextOffset)
	}
}

func TestPagePrevClampsToZero(t *testing.T) {
	exec := &fakeExec{rows: rows(1)}
	page, err := New(exec).PageFromForm(context.Background(), FormFilters{Limit: "10", Offset: "4"})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if !page.HasPrev || page.PrevOffset != 0 {
		t.Errorf("prev must clamp to zero: %+v", page)
	}
}

func TestFormValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		filters FormFilters
		rule    string
		detail  string
	}{
		{"severity", FormFilters{Severity: "boom"}, "severity", "unknown severity \"boom\" (expected DEBUG, INFO, WARN, ERROR, or FATAL)"},
		{"date_from", FormFilters{DateFrom: "garbage"}, "date_from", "invalid date_from: cannot parse date: \"garbage\""},
		{"date_to", FormFilters{DateTo: "15/01/2024"}, "date_to", "invalid date_to: cannot parse date: \"15/01/2024\""},
		{"limit", FormFilters{Limit: "-1"}, "limit", "limit must be an integer between 1 and 1000, or 0 for the default"},
		{"limit_not_int", FormFilters{Limit: "many"}, "limit", "limit must be an integer between 1 and 1000, or 0 for the default"},
		{"offset", FormFilters{Offset: "-4"}, "offset", "offset must be a non-negative integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(&fakeExec{}).PageFromForm(context.Background(), tc.filters)
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("expected ValidationError, got %v", err)
			}
			if ve.Rule != tc.rule {
				t.Errorf("rule: got %q, want %q", ve.Rule, tc.rule)
			}
			if ve.Detail != tc.detail {
				t.Errorf("detail: got %q, want %q", ve.Detail, tc.detail)
			}
		})
	}
}

func TestFormValidationErrorFraming(t *testing.T) {
	// Byte-identity guard: the form path renders limit/offset Detail alone
	// (legacy was never prefixed); every other rule renders Error() with the
	// "validation: " prefix exactly as the legacy handlers did.
	_, err := New(&fakeExec{}).PageFromForm(context.Background(), FormFilters{Limit: "-1"})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
	if ve.Detail == ve.Error() || !strings.HasPrefix(ve.Error(), "validation: ") {
		t.Errorf("unexpected framing: detail=%q error=%q", ve.Detail, ve.Error())
	}
	_, err = New(&fakeExec{}).PageFromForm(context.Background(), FormFilters{Severity: "boom"})
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
	if !strings.HasPrefix(ve.Error(), "validation: unknown severity") {
		t.Errorf("severity framing must keep the legacy prefix: %q", ve.Error())
	}
}

func TestDSLValidationWrapsByteIdentical(t *testing.T) {
	_, err := New(&fakeExec{}).PageFromDSL(context.Background(), dsl.Query{Limit: 5000})
	if err == nil || err.Error() != "validation: limit 5000 exceeds maximum of 1000" {
		t.Fatalf("byte-identity broken: %v", err)
	}
}

func TestDSLValidationRejectsBadField(t *testing.T) {
	q := dsl.Query{Where: dsl.BinaryExpr{Op: dsl.OpEq, Field: "bogus", Value: dsl.Value{Type: dsl.ValueString, String: "x"}}}
	_, err := New(&fakeExec{}).PageFromDSL(context.Background(), q)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Rule != "dsl" {
		t.Fatalf("expected dsl ValidationError, got %v", err)
	}
	if !strings.HasPrefix(ve.Error(), "validation: where:") {
		t.Errorf("dsl framing must keep the legacy prefix: %q", ve.Error())
	}
}

func TestMatchSyntaxRejectedThroughExecutor(t *testing.T) {
	exec := &fakeExec{hasFTS: true, validateErr: errors.New("unterminated string")}
	_, err := New(exec).PageFromForm(context.Background(), FormFilters{Search: "unterminated"})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
	if ve.Rule != "match" {
		t.Errorf("rule: got %q, want match", ve.Rule)
	}
	if !strings.HasPrefix(ve.Detail, "invalid full-text search \"unterminated\": unterminated string") {
		t.Errorf("unexpected match detail: %q", ve.Detail)
	}
}

func TestExecuteErrorsAreNotValidation(t *testing.T) {
	exec := &fakeExec{executeErr: errors.New("boom")}
	_, err := New(exec).PageFromForm(context.Background(), FormFilters{})
	var ve *ValidationError
	if errors.As(err, &ve) {
		t.Fatalf("execute failures must not be validation errors: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "execute: ") {
		t.Errorf("execute failures keep their legacy framing: %v", err)
	}
}

func TestFormEchoCanonicalizes(t *testing.T) {
	exec := &fakeExec{rows: rows(1)}
	page, err := New(exec).PageFromForm(context.Background(), FormFilters{Severity: "error", Since: " 1h", Limit: "7"})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if page.Echo.Severity != "ERROR" {
		t.Errorf("severity echo: got %q, want ERROR", page.Echo.Severity)
	}
	if page.Echo.Limit != 7 {
		t.Errorf("limit echo: got %d, want 7 (effective)", page.Echo.Limit)
	}
	// Since is preserved raw for the form echo (Normalize consumes the
	// parseable form; the raw value is what the form re-renders).
	if page.Echo.Since != " 1h" {
		t.Errorf("since echo must preserve the raw form value: %q", page.Echo.Since)
	}
}

func TestDefaultLimitEcho(t *testing.T) {
	exec := &fakeExec{rows: rows(1)}
	page, err := New(exec).PageFromForm(context.Background(), FormFilters{})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if page.Echo.Limit != dsl.DefaultLimit || page.Echo.Offset != 0 {
		t.Errorf("echo must carry effective defaults: %+v", page.Echo)
	}
}
