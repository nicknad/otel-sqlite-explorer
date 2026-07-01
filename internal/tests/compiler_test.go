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

	// Expected: normalized adds default ORDER BY timestamp DESC.
	// Logical expressions are wrapped in parentheses by the compiler.
	expectedSQL := "SELECT id, timestamp, severity, service_name, trace_id, span_id, body " +
		"FROM logs WHERE (service_name = ? AND body LIKE ?) " +
		"ORDER BY timestamp DESC LIMIT ? OFFSET ?"
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
		dsl.Validate(q)
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
	json.Unmarshal([]byte(input), &q)
	dsl.Validate(&q)
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
	if !strings.Contains(cq.SQL, "logs.id, logs.timestamp") {
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
	json.Unmarshal([]byte(input), &q1)
	json.Unmarshal([]byte(input), &q2)
	dsl.Validate(&q1)
	dsl.Validate(&q2)
	dsl.Normalize(&q1)
	dsl.Normalize(&q2)
	c1, _ := compiler.Compile(&q1)
	c2, _ := compiler.Compile(&q2)
	if c1.SQL != c2.SQL || !reflect.DeepEqual(c1.Args, c2.Args) {
		t.Errorf("non-deterministic match compilation")
	}
}
