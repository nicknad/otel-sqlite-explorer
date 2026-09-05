package tests

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"log-explorer/internal/compiler"
	"log-explorer/internal/dsl"
)

func TestCompilerDeterminism(t *testing.T) {
	input := `{
		"where": {
			"and": [
				{"eq": ["service_name", "api"]},
				{"contains": ["body", "timeout"]}
			]
		},
		"limit": 100
	}`

	var q dsl.Query
	if err := json.Unmarshal([]byte(input), &q); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if err := dsl.Validate(&q); err != nil {
		t.Fatalf("validate: %v", err)
	}

	dsl.Normalize(&q)

	cq, err := compiler.Compile(&q)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// Expected: normalized adds default ORDER BY timestamp_ns DESC.
	// Logical expressions are wrapped in parentheses by the compiler.
	// LIKE patterns escape metacharacters with ESCAPE '\'.
	expectedSQL := "SELECT id, timestamp_ns, severity_text, severity_number, service_name, lower(hex(trace_id)), lower(hex(span_id)), body, attributes_json " +
		"FROM logs WHERE (service_name = ? AND body LIKE ? ESCAPE '\\') " +
		"ORDER BY timestamp_ns DESC LIMIT ? OFFSET ?"
	expectedArgs := []any{"api", "%timeout%", 100, 0}

	if cq.SQL != expectedSQL {
		t.Errorf("SQL mismatch:\ngot:  %s\nwant: %s", cq.SQL, expectedSQL)
	}

	if !reflect.DeepEqual(cq.Args, expectedArgs) {
		t.Errorf("args mismatch:\ngot:  %v\nwant: %v", cq.Args, expectedArgs)
	}
}

func TestCompilerDeterministicIdentity(t *testing.T) {
	// Same input must produce identical output on repeated calls.
	input := `{
		"where": {"eq": ["severity", "ERROR"]},
		"sort": [{"field": "timestamp", "desc": true}],
		"limit": 50
	}`

	var q1, q2 dsl.Query
	if err := json.Unmarshal([]byte(input), &q1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(input), &q2); err != nil {
		t.Fatal(err)
	}

	for _, q := range []*dsl.Query{&q1, &q2} {
		_ = dsl.Validate(q)
		dsl.Normalize(q)
	}

	c1, _ := compiler.Compile(&q1)
	c2, _ := compiler.Compile(&q2)

	if c1.SQL != c2.SQL {
		t.Errorf("non-deterministic SQL:\n1: %s\n2: %s", c1.SQL, c2.SQL)
	}
	if !reflect.DeepEqual(c1.Args, c2.Args) {
		t.Errorf("non-deterministic args:\n1: %v\n2: %v", c1.Args, c2.Args)
	}
}

func TestCompilerNoInlineValues(t *testing.T) {
	input := `{"where": {"eq": ["body", "test"]}, "limit": 10}`
	var q dsl.Query
	_ = json.Unmarshal([]byte(input), &q)
	_ = dsl.Validate(&q)
	dsl.Normalize(&q)

	cq, err := compiler.Compile(&q)
	if err != nil {
		t.Fatal(err)
	}

	// Verify no inline value — only `?` placeholders.
	for _, r := range cq.SQL {
		if r == '\'' || r == '"' {
			t.Errorf("SQL contains inline quote character: %s", cq.SQL)
			break
		}
	}
}

func TestCompilerMatchCompilesToFTS(t *testing.T) {
	input := `{"where": {"match": "timeout gateway"}, "limit": 50}`
	var q dsl.Query
	if err := json.Unmarshal([]byte(input), &q); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := dsl.Validate(&q); err != nil {
		t.Fatalf("validate: %v", err)
	}
	dsl.Normalize(&q)

	cq, err := compiler.Compile(&q)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	if !strings.Contains(cq.SQL, "JOIN logs_fts ON logs.id = logs_fts.rowid") {
		t.Errorf("expected FTS5 JOIN, got: %s", cq.SQL)
	}
	if !strings.Contains(cq.SQL, "logs_fts MATCH ?") {
		t.Errorf("expected logs_fts MATCH ?, got: %s", cq.SQL)
	}
	if !strings.Contains(cq.SQL, "ORDER BY bm25(logs_fts) ASC") {
		t.Errorf("expected bm25 ranking, got: %s", cq.SQL)
	}
	if !strings.Contains(cq.SQL, "logs.id, logs.timestamp_ns") {
		t.Errorf("expected qualified columns, got: %s", cq.SQL)
	}
	expectedArgs := []any{"timeout gateway", 50, 0}
	if !reflect.DeepEqual(cq.Args, expectedArgs) {
		t.Errorf("args mismatch:\ngot:  %v\nwant: %v", cq.Args, expectedArgs)
	}
}

func TestCompilerHybridMatchAndStructured(t *testing.T) {
	input := `{
		"where": {
			"and": [
				{"eq": ["service_name", "api"]},
				{"match": "timeout"}
			]
		},
		"limit": 100
	}`
	var q dsl.Query
	if err := json.Unmarshal([]byte(input), &q); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := dsl.Validate(&q); err != nil {
		t.Fatalf("validate: %v", err)
	}
	dsl.Normalize(&q)

	cq, err := compiler.Compile(&q)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	if !strings.Contains(cq.SQL, "logs_fts MATCH ?") {
		t.Errorf("missing MATCH: %s", cq.SQL)
	}
	if !strings.Contains(cq.SQL, "logs.service_name = ?") {
		t.Errorf("missing structured filter (qualified): %s", cq.SQL)
	}
	expectedArgs := []any{"api", "timeout", 100, 0}
	if !reflect.DeepEqual(cq.Args, expectedArgs) {
		t.Errorf("args mismatch:\ngot:  %v\nwant: %v", cq.Args, expectedArgs)
	}
}

func TestCompilerMatchDeterministic(t *testing.T) {
	input := `{"where": {"match": "error timeout"}, "limit": 10}`
	var q1, q2 dsl.Query
	_ = json.Unmarshal([]byte(input), &q1)
	_ = json.Unmarshal([]byte(input), &q2)
	_ = dsl.Validate(&q1)
	_ = dsl.Validate(&q2)
	dsl.Normalize(&q1)
	dsl.Normalize(&q2)
	c1, _ := compiler.Compile(&q1)
	c2, _ := compiler.Compile(&q2)
	if c1.SQL != c2.SQL || !reflect.DeepEqual(c1.Args, c2.Args) {
		t.Errorf("non-deterministic match compilation")
	}
}

func TestCompilerPartialSelect(t *testing.T) {
	input := `{"select": "severity, body", "limit": 10}`
	var q dsl.Query
	if err := json.Unmarshal([]byte(input), &q); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := dsl.Validate(&q); err != nil {
		t.Fatalf("validate: %v", err)
	}
	dsl.Normalize(&q)

	cq, err := compiler.Compile(&q)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	wantSQL := "SELECT severity_text, body FROM logs ORDER BY timestamp_ns DESC LIMIT ? OFFSET ?"
	if cq.SQL != wantSQL {
		t.Errorf("SQL mismatch:\ngot:  %s\nwant: %s", cq.SQL, wantSQL)
	}
	wantFields := []string{"severity", "body"}
	if !reflect.DeepEqual(cq.Fields, wantFields) {
		t.Errorf("fields mismatch:\ngot:  %v\nwant: %v", cq.Fields, wantFields)
	}
}

func TestCompilerGetByID(t *testing.T) {
	cq := compiler.CompileGetByID(42)
	want := "SELECT id, timestamp_ns, severity_text, severity_number, service_name, lower(hex(trace_id)), lower(hex(span_id)), body, attributes_json " +
		"FROM logs WHERE id = ? LIMIT 1"
	if cq.SQL != want {
		t.Errorf("SQL mismatch:\ngot:  %s\nwant: %s", cq.SQL, want)
	}
	if !reflect.DeepEqual(cq.Args, []any{int64(42)}) {
		t.Errorf("args mismatch: %v", cq.Args)
	}
}

func TestCompilerInjectionAttemptIsParameterized(t *testing.T) {
	payload := `{"where": {"eq": ["body", "' OR '1'='1"]}, "limit": 10}`
	var q dsl.Query
	if err := json.Unmarshal([]byte(payload), &q); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := dsl.Validate(&q); err != nil {
		t.Fatalf("validate: %v", err)
	}
	dsl.Normalize(&q)

	cq, err := compiler.Compile(&q)
	if err != nil {
		t.Fatal(err)
	}

	// The hostile text must never appear in the SQL string; it travels only
	// as a bound argument.
	if strings.Contains(cq.SQL, "' OR ") {
		t.Errorf("SQL contains raw injection value: %s", cq.SQL)
	}
	if !strings.Contains(cq.SQL, "body = ?") {
		t.Errorf("expected parameterized body comparison, got: %s", cq.SQL)
	}
	if len(cq.Args) != 3 || cq.Args[0] != "' OR '1'='1" {
		t.Errorf("expected injection string as first bound arg, got %v", cq.Args)
	}
}

func TestCompilerContainsEscapesWildcards(t *testing.T) {
	input := `{"where": {"contains": ["body", "100%_ok\\done"]}, "limit": 10}`
	var q dsl.Query
	if err := json.Unmarshal([]byte(input), &q); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := dsl.Validate(&q); err != nil {
		t.Fatalf("validate: %v", err)
	}
	dsl.Normalize(&q)

	cq, err := compiler.Compile(&q)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !strings.Contains(cq.SQL, "LIKE ? ESCAPE '\\'") {
		t.Errorf("expected ESCAPE clause, got: %s", cq.SQL)
	}
	want := `%100\%\_ok\\done%`
	if len(cq.Args) == 0 || cq.Args[0] != want {
		t.Errorf("expected escaped pattern %q, got %v", want, cq.Args)
	}
}

func TestCompilerBlobFilterIsCaseInsensitive(t *testing.T) {
	// Uppercase hex trace ids must match the lower(hex(...)) expression.
	input := `{"where": {"eq": ["trace_id", "00112233445566778899AABBCCDDEEFF"]}, "limit": 10}`
	var q dsl.Query
	if err := json.Unmarshal([]byte(input), &q); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := dsl.Validate(&q); err != nil {
		t.Fatalf("validate: %v", err)
	}
	dsl.Normalize(&q)

	cq, err := compiler.Compile(&q)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(cq.Args) == 0 || cq.Args[0] != "00112233445566778899aabbccddeeff" {
		t.Errorf("expected lowercased hex arg, got %v", cq.Args)
	}
}

func TestCompilerDefaultsWithoutNormalize(t *testing.T) {
	// Callers that bypass Normalize still get a deterministic ORDER BY and
	// sane LIMIT/OFFSET so pagination never silently returns nothing.
	q := dsl.Query{Select: "*"}
	cq, err := compiler.Compile(&q)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !strings.Contains(cq.SQL, "ORDER BY timestamp_ns DESC") {
		t.Errorf("expected default timestamp ordering, got: %s", cq.SQL)
	}
	wantArgs := []any{dsl.DefaultLimit, 0}
	if !reflect.DeepEqual(cq.Args, wantArgs) {
		t.Errorf("expected default limit/offset %v, got %v", wantArgs, cq.Args)
	}
}
