package tests

import (
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"log-explorer/internal/compiler"
	"log-explorer/internal/db"
	"log-explorer/internal/dsl"
	"log-explorer/internal/migrate"
)

func setupTestDB(t *testing.T) string {
	t.Helper()

	f, err := os.CreateTemp("", "logexplorer-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	path := f.Name()
	f.Close()

	// Open with write access to create schema and seed data.
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		os.Remove(path)
		t.Fatalf("open write db: %v", err)
	}

	schema := `CREATE TABLE logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp_ns INTEGER,
		severity_text TEXT,
		service_name TEXT,
		trace_id BLOB,
		span_id BLOB,
		body TEXT
	);`

	if _, err := conn.Exec(schema); err != nil {
		conn.Close()
		os.Remove(path)
		t.Fatalf("create table: %v", err)
	}

	// Insert sample rows.
	rows := []struct {
		ts          int64
		severity    string
		serviceName string
		traceID     string
		spanID      string
		body        string
	}{
		{ts: 1_700_000_000_000_000_000, severity: "ERROR", serviceName: "api-gateway",
			traceID: "abc123", spanID: "span1", body: "connection timeout to upstream"},
		{ts: 1_700_000_000_000_000_001, severity: "INFO", serviceName: "api-gateway",
			traceID: "abc123", spanID: "span2", body: "request completed"},
		{ts: 1_700_000_000_000_000_002, severity: "WARN", serviceName: "auth-svc",
			traceID: "def456", spanID: "span3", body: "rate limit approaching"},
		{ts: 1_700_000_000_000_000_003, severity: "ERROR", serviceName: "auth-svc",
			traceID: "def456", spanID: "span4", body: "token validation failed"},
		{ts: 1_700_000_000_000_000_004, severity: "DEBUG", serviceName: "api-gateway",
			traceID: "ghi789", spanID: "span5", body: "debug: parsed headers"},
	}

	for _, r := range rows {
		_, err := conn.Exec(
			"INSERT INTO logs (timestamp_ns, severity_text, service_name, trace_id, span_id, body) VALUES (?, ?, ?, ?, ?, ?)",
			r.ts, r.severity, r.serviceName, []byte(r.traceID), []byte(r.spanID), r.body,
		)
		if err != nil {
			conn.Close()
			os.Remove(path)
			t.Fatalf("insert: %v", err)
		}
	}

	conn.Close()
	return path
}

// setupTestDBWithFTS is like setupTestDB but also creates and populates an FTS5
// index (logs_fts) over body and service_name, using the same migrate package
// the production maintenance tool uses, so MatchExpr queries can be exercised
// end-to-end. No triggers are created.
func setupTestDBWithFTS(t *testing.T) string {
	t.Helper()
	path := setupTestDB(t)

	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open for fts: %v", err)
	}
	defer conn.Close()

	if _, err := migrate.RebuildFTS(conn); err != nil {
		t.Fatalf("rebuild fts: %v", err)
	}
	return path
}

func TestIntegrationQueryFullStack(t *testing.T) {
	path := setupTestDB(t)
	defer os.Remove(path)

	// Open read-only via the db package.
	client, err := db.Open(path)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	defer client.Close()

	// Build a DSL query: service_name contains "gateway" AND severity = "ERROR".
	input := `{
		"where": {
			"and": [
				{"contains": ["service_name", "gateway"]},
				{"eq": ["severity", "ERROR"]}
			]
		},
		"sort": [{"field": "timestamp", "desc": false}],
		"limit": 10
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

	results, err := client.Execute(cq)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Expect exactly 1 row: api-gateway / ERROR / "connection timeout to upstream"
	if len(results) != 1 {
		t.Fatalf("expected 1 row, got %d", len(results))
	}

	r := results[0]
	if r.ServiceName != "api-gateway" {
		t.Errorf("expected service_name=api-gateway, got %s", r.ServiceName)
	}
	if r.Severity != "ERROR" {
		t.Errorf("expected severity=ERROR, got %s", r.Severity)
	}
	if r.Body != "connection timeout to upstream" {
		t.Errorf("expected body='connection timeout to upstream', got %s", r.Body)
	}
}

func TestIntegrationEmptyWhere(t *testing.T) {
	path := setupTestDB(t)
	defer os.Remove(path)

	client, err := db.Open(path)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	defer client.Close()

	// No WHERE clause — should return all rows (limited to 100).
	q := dsl.Query{Select: "*", Limit: 100, Offset: 0}
	q.Sort = []dsl.Sort{{Field: "timestamp", Desc: false}}
	dsl.Validate(&q)
	dsl.Normalize(&q)
	dsl.Validate(&q) // validate again after normalize

	cq, err := compiler.Compile(&q)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	results, err := client.Execute(cq)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if len(results) != 5 {
		t.Fatalf("expected 5 rows, got %d", len(results))
	}
}

func TestIntegrationFTSMatch(t *testing.T) {
	path := setupTestDBWithFTS(t)
	defer os.Remove(path)

	client, err := db.Open(path)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	defer client.Close()
	if !client.HasFTS() {
		t.Fatal("expected HasFTS=true")
	}

	// FTS5 tokenizes "connection timeout to upstream" into [connection, timeout, to, upstream].
	// Querying "timeout" (a single token) must match that row.
	input := `{"where": {"match": "timeout"}, "limit": 10}`
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
	results, err := client.Execute(cq)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 row matching 'timeout', got %d: %+v", len(results), results)
	}
	if !strings.Contains(results[0].Body, "timeout") {
		t.Errorf("unexpected body: %s", results[0].Body)
	}
}

func TestIntegrationFTSHybridMatchStructured(t *testing.T) {
	path := setupTestDBWithFTS(t)
	defer os.Remove(path)

	client, err := db.Open(path)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	defer client.Close()

	// Full-text "timeout" AND service_name == "auth-svc" should match nothing
	// (the timeout row belongs to api-gateway), while "failed" AND auth-svc
	// should match exactly the token-validation row.
	input := `{
		"where": {
			"and": [
				{"eq": ["service_name", "auth-svc"]},
				{"match": "failed"}
			]
		},
		"limit": 10
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
	results, err := client.Execute(cq)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 hybrid row, got %d: %+v", len(results), results)
	}
	if results[0].ServiceName != "auth-svc" || !strings.Contains(results[0].Body, "failed") {
		t.Errorf("unexpected row: %+v", results[0])
	}
}

func TestIntegrationFTSFallbackWithoutIndex(t *testing.T) {
	// A plain DB (no logs_fts) must fall back match -> body contains.
	path := setupTestDB(t)
	defer os.Remove(path)

	client, err := db.Open(path)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	defer client.Close()
	if client.HasFTS() {
		t.Fatal("expected HasFTS=false for plain db")
	}

	q := dsl.Query{
		Where: dsl.MatchExpr{Query: "timeout"},
		Limit: 10,
	}
	if err := dsl.Validate(&q); err != nil {
		t.Fatalf("validate: %v", err)
	}
	q.Where = dsl.RewriteMatchToContains(q.Where)
	dsl.Normalize(&q)

	cq, err := compiler.Compile(&q)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	results, err := client.Execute(cq)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(results) != 1 || !strings.Contains(results[0].Body, "timeout") {
		t.Fatalf("fallback substring match failed, got %d rows: %+v", len(results), results)
	}
}
