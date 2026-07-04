package tests

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"log-explorer/internal/compiler"
	"log-explorer/internal/db"
	"log-explorer/internal/dsl"
	"log-explorer/internal/migrate"

	_ "modernc.org/sqlite"
)

// trace1 / trace2 are 16-byte trace IDs whose hex representation is the
// value shown in the UI so that trace_id filters work end-to-end.
var (
	trace1, _ = hex.DecodeString("00112233445566778899aabbccddeeff")
	trace2, _ = hex.DecodeString("ffeeddccbbaa99887766554433221100")
	trace3, _ = hex.DecodeString("a1b2c3d4e5f60718293a4b5c6d7e8f90")

	span1, _ = hex.DecodeString("0a1b2c3d4e5f6071")
	span2, _ = hex.DecodeString("0a1b2c3d4e5f6072")
	span3, _ = hex.DecodeString("0a1b2c3d4e5f6073")
	span4, _ = hex.DecodeString("0a1b2c3d4e5f6074")
	span5, _ = hex.DecodeString("0a1b2c3d4e5f6075")
)

func setupTestDB(t *testing.T) string {
	t.Helper()

	f, err := os.CreateTemp("", "logexplorer-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	path := f.Name()
	_ = f.Close()

	conn, err := sql.Open("sqlite", path)
	if err != nil {
		_ = os.Remove(path)
		t.Fatalf("open write db: %v", err)
	}

	// Match the production schema exactly: log_resource + log_event + log_attr
	// + logs VIEW (which joins log_event and log_resource).
	schema := `
		CREATE TABLE log_resource (
			id TEXT PRIMARY KEY,
			service_name TEXT NOT NULL,
			host_name TEXT,
			schema_url TEXT,
			attributes_json TEXT
		);

		CREATE TABLE log_event (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			resource_id TEXT NOT NULL,
			timestamp_ns INTEGER NOT NULL,
			observed_timestamp_ns INTEGER NOT NULL,
			severity_number INTEGER NOT NULL,
			severity_text TEXT,
			trace_id BLOB,
			span_id BLOB,
			body TEXT,
			event_name TEXT,
			flags INTEGER NOT NULL DEFAULT 0,
			dropped_attributes_count INTEGER NOT NULL DEFAULT 0,
			scope_name TEXT,
			scope_version TEXT,
			FOREIGN KEY (resource_id) REFERENCES log_resource(id)
		);

		CREATE TABLE log_attr (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			event_id INTEGER NOT NULL,
			key TEXT NOT NULL,
			value_type TEXT NOT NULL,
			string_value TEXT,
			int_value INTEGER,
			double_value REAL,
			bool_value INTEGER,
			bytes_value BLOB,
			FOREIGN KEY (event_id) REFERENCES log_event(id) ON DELETE CASCADE
		);

		CREATE VIEW logs AS
		SELECT
			le.id                AS id,
			le.timestamp_ns     AS timestamp_ns,
			le.severity_text     AS severity_text,
			le.severity_number   AS severity_number,
			le.trace_id          AS trace_id,
			le.span_id           AS span_id,
			le.body              AS body,
			lr.service_name      AS service_name
		FROM log_event le
		JOIN log_resource lr ON le.resource_id = lr.id;`

	if _, err := conn.Exec(schema); err != nil {
		_ = conn.Close()
		_ = os.Remove(path)
		t.Fatalf("create schema: %v", err)
	}

	// Insert resources.
	resources := []struct {
		id, serviceName string
	}{
		{"res-api", "api-gateway"},
		{"res-auth", "auth-svc"},
	}
	for _, r := range resources {
		if _, err := conn.Exec(
			"INSERT INTO log_resource (id, service_name) VALUES (?, ?)",
			r.id, r.serviceName,
		); err != nil {
			_ = conn.Close()
			_ = os.Remove(path)
			t.Fatalf("insert resource: %v", err)
		}
	}

	// Insert sample log events.
	type logRow struct {
		ts       int64
		resource string
		severity string
		sevNum   int64
		traceID  []byte
		spanID   []byte
		body     string
	}
	rows := []logRow{
		{1_700_000_000_000_000_000, "res-api", "ERROR", 17, trace1, span1, "connection timeout to upstream"},
		{1_700_000_000_000_000_001, "res-api", "INFO", 9, trace1, span2, "request completed"},
		{1_700_000_000_000_000_002, "res-auth", "WARN", 13, trace2, span3, "rate limit approaching"},
		{1_700_000_000_000_000_003, "res-auth", "ERROR", 17, trace2, span4, "token validation failed"},
		{1_700_000_000_000_000_004, "res-api", "DEBUG", 5, trace3, span5, "debug: parsed headers"},
	}

	for _, r := range rows {
		_, err := conn.Exec(
			`INSERT INTO log_event
				(timestamp_ns, observed_timestamp_ns, resource_id,
				 severity_number, severity_text, trace_id, span_id, body)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ts, r.ts, r.resource,
			r.sevNum, r.severity, r.traceID, r.spanID, r.body,
		)
		if err != nil {
			_ = conn.Close()
			_ = os.Remove(path)
			t.Fatalf("insert log event: %v", err)
		}
	}

	_ = conn.Close()
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
	defer func() { _ = conn.Close() }()

	if _, err := migrate.RebuildFTS(conn); err != nil {
		t.Fatalf("rebuild fts: %v", err)
	}
	return path
}

func TestIntegrationQueryFullStack(t *testing.T) {
	path := setupTestDB(t)
	defer func() { _ = os.Remove(path) }()

	// Open read-only via the db package.
	client, err := db.Open(path)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	defer func() { _ = client.Close() }()

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
	err = json.Unmarshal([]byte(input), &q)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	err = dsl.Validate(&q)
	if err != nil {
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
	defer func() { _ = os.Remove(path) }()

	client, err := db.Open(path)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	defer func() { _ = client.Close() }()

	// No WHERE clause — should return all rows (limited to 100).
	q := dsl.Query{Select: "*", Limit: 100, Offset: 0}
	q.Sort = []dsl.Sort{{Field: "timestamp", Desc: false}}
	_ = dsl.Validate(&q)
	dsl.Normalize(&q)
	_ = dsl.Validate(&q) // validate again after normalize

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
	defer func() { _ = os.Remove(path) }()

	client, err := db.Open(path)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	defer func() { _ = client.Close() }()
	if !client.HasFTS() {
		t.Fatal("expected HasFTS=true")
	}

	// FTS5 tokenizes "connection timeout to upstream" into [connection, timeout, to, upstream].
	// Querying "timeout" (a single token) must match that row.
	input := `{"where": {"match": "timeout"}, "limit": 10}`
	var q dsl.Query
	err = json.Unmarshal([]byte(input), &q)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	err = dsl.Validate(&q)
	if err != nil {
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
	defer func() { _ = os.Remove(path) }()

	client, err := db.Open(path)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	defer func() { _ = client.Close() }()

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
	err = json.Unmarshal([]byte(input), &q)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	err = dsl.Validate(&q)
	if err != nil {
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
	defer func() { _ = os.Remove(path) }()

	client, err := db.Open(path)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	defer func() { _ = client.Close() }()
	if client.HasFTS() {
		t.Fatal("expected HasFTS=false for plain db")
	}

	q := dsl.Query{
		Where: dsl.MatchExpr{Query: "timeout"},
		Limit: 10,
	}
	err = dsl.Validate(&q)
	if err != nil {
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
