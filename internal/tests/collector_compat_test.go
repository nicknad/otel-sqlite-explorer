package tests

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"log-explorer/internal/compiler"
	"log-explorer/internal/db"
	"log-explorer/internal/dsl"

	_ "modernc.org/sqlite"
)

// collectorDDL mirrors the refactored otel-sqlite collector schema (000 +
// 003 + 004): superset logs view (18 cols), 6-column contentless logs_fts
// with maintenance triggers, composite hot-path indexes, and the extra
// scope/metric tables the explorer must ignore. The explorer only SELECTs
// its 9-column subset, so this must keep working without code changes.
const collectorDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version TEXT PRIMARY KEY,
    applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    description TEXT
);
CREATE TABLE IF NOT EXISTS log_resource (
    id TEXT PRIMARY KEY,
    service_name TEXT NOT NULL,
    host_name TEXT,
    schema_url TEXT,
    attributes_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_log_resource_service ON log_resource(service_name);
CREATE TABLE IF NOT EXISTS log_event (
    id INTEGER PRIMARY KEY,
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
    scope_attributes_json TEXT NOT NULL DEFAULT '{}',
    scope_schema_url TEXT,
    attributes_json TEXT NOT NULL DEFAULT '{}',
    FOREIGN KEY (resource_id) REFERENCES log_resource(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_log_event_timestamp ON log_event(timestamp_ns);
CREATE INDEX IF NOT EXISTS idx_log_event_resource_timestamp ON log_event(resource_id, timestamp_ns);
CREATE INDEX IF NOT EXISTS idx_log_event_trace_timestamp ON log_event(trace_id, timestamp_ns) WHERE trace_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_log_event_sev ON log_event(severity_number);
CREATE VIEW IF NOT EXISTS logs AS
SELECT le.id, le.resource_id, le.timestamp_ns, le.observed_timestamp_ns,
    le.severity_number, le.severity_text, le.trace_id, le.span_id,
    le.body, le.event_name, le.flags, le.dropped_attributes_count,
    le.scope_name, le.scope_version, le.scope_attributes_json, le.scope_schema_url,
    le.attributes_json, lr.service_name, lr.host_name, lr.schema_url AS resource_schema_url
FROM log_event AS le JOIN log_resource AS lr ON lr.id = le.resource_id;
CREATE VIRTUAL TABLE IF NOT EXISTS logs_fts USING fts5(
    body, service_name, host_name, severity_text, event_name, scope_name,
    tokenize='unicode61 remove_diacritics 2', content='', contentless_delete=1
);
CREATE TRIGGER logs_fts_ai AFTER INSERT ON log_event BEGIN
    INSERT INTO logs_fts(rowid,body,service_name,host_name,severity_text,event_name,scope_name)
    SELECT new.id, COALESCE(new.body,''), COALESCE(lr.service_name,''),
        COALESCE(lr.host_name,''), COALESCE(new.severity_text,''),
        COALESCE(new.event_name,''), COALESCE(new.scope_name,'')
    FROM log_resource AS lr WHERE lr.id=new.resource_id;
END;
CREATE TRIGGER logs_fts_ad AFTER DELETE ON log_event BEGIN
    DELETE FROM logs_fts WHERE rowid=old.id;
END;
CREATE TABLE IF NOT EXISTS scope (
    id TEXT PRIMARY KEY,
    resource_id TEXT NOT NULL,
    name TEXT, version TEXT, schema_url TEXT,
    attributes_json TEXT NOT NULL DEFAULT '{}',
    FOREIGN KEY (resource_id) REFERENCES log_resource(id) ON DELETE CASCADE,
    UNIQUE(resource_id, name, version, schema_url)
);
CREATE TABLE IF NOT EXISTS metric (
    id TEXT PRIMARY KEY,
    scope_id TEXT NOT NULL,
    name TEXT NOT NULL, description TEXT, unit TEXT,
    type INTEGER NOT NULL,
    is_monotonic INTEGER NOT NULL DEFAULT 0 CHECK(is_monotonic IN (0,1)),
    aggregation_temporality INTEGER NOT NULL DEFAULT 0,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    FOREIGN KEY (scope_id) REFERENCES scope(id) ON DELETE CASCADE,
    UNIQUE(scope_id, name, unit, type)
);
`

// setupCollectorDB builds a temp DB using the collector's DDL (not the
// explorer's legacy test DDL), populates it via the collector's triggers
// (no manual FTS backfill), and returns the path.
func setupCollectorDB(t *testing.T) string {
	t.Helper()

	f, err := os.CreateTemp("", "collector-compat-*.db")
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
	defer func() { _ = conn.Close() }()

	if _, err := conn.Exec(collectorDDL); err != nil {
		_ = os.Remove(path)
		t.Fatalf("create collector schema: %v", err)
	}

	// Legacy-shaped minimal inserts must still work: new NOT NULL columns
	// all carry defaults.
	if _, err := conn.Exec(
		`INSERT INTO log_resource (id, service_name) VALUES ('res-api', 'api-gateway'), ('res-auth', 'auth-svc')`,
	); err != nil {
		_ = os.Remove(path)
		t.Fatalf("insert resources: %v", err)
	}
	rows := []struct {
		ts       int64
		resource string
		sevNum   int64
		sevText  string
		traceID  []byte
		spanID   []byte
		body     string
	}{
		{1_700_000_000_000_000_000, "res-api", 17, "ERROR", trace1, span1, "connection timeout to upstream"},
		{1_700_000_000_000_000_001, "res-api", 9, "INFO", trace1, span2, "request completed"},
		{1_700_000_000_000_000_003, "res-auth", 17, "ERROR", trace2, span4, "token validation failed"},
	}
	for _, r := range rows {
		if _, err := conn.Exec(
			`INSERT INTO log_event
				(timestamp_ns, observed_timestamp_ns, resource_id,
				 severity_number, severity_text, trace_id, span_id, body)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ts, r.ts, r.resource, r.sevNum, r.sevText, r.traceID, r.spanID, r.body,
		); err != nil {
			_ = os.Remove(path)
			t.Fatalf("insert log event: %v", err)
		}
	}
	// Extra collector tables exist but stay empty; the explorer must ignore them.
	return path
}

func openCollectorRO(t *testing.T, path string) *db.Client {
	t.Helper()
	client, err := db.Open(path)
	if err != nil {
		_ = os.Remove(path)
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = os.Remove(path)
	})
	return client
}

// The superset logs view must expose every column the explorer SELECTs.
func TestCollectorCompat_ViewColumns(t *testing.T) {
	path := setupCollectorDB(t)
	defer func() { _ = os.Remove(path) }()

	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = conn.Close() }()

	for _, c := range []string{"id", "timestamp_ns", "severity_text", "severity_number",
		"trace_id", "span_id", "body", "attributes_json", "service_name"} {
		var n int
		if err := conn.QueryRow(
			`SELECT count(*) FROM pragma_table_info('logs') WHERE name = ?`, c,
		).Scan(&n); err != nil {
			t.Fatalf("pragma logs %s: %v", c, err)
		}
		if n != 1 {
			t.Errorf("logs view missing explorer column %q", c)
		}
	}
}

// Full scan + structured filters + detail lookup against the new schema.
func TestCollectorCompat_ReadPaths(t *testing.T) {
	client := openCollectorRO(t, setupCollectorDB(t))
	if !client.HasFTS() {
		t.Fatal("expected HasFTS=true on collector schema")
	}

	run := func(q *dsl.Query) []db.LogRow {
		t.Helper()
		if err := dsl.Validate(q); err != nil {
			t.Fatalf("validate: %v", err)
		}
		dsl.Normalize(q)
		cq, err := compiler.Compile(q)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		rows, err := client.Execute(cq)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		return rows
	}

	if rows := run(&dsl.Query{Limit: 100}); len(rows) != 3 {
		t.Fatalf("full scan: got %d rows, want 3", len(rows))
	}
	q := &dsl.Query{
		Where: dsl.BinaryExpr{Op: dsl.OpContains, Field: "service_name",
			Value: dsl.Value{Type: dsl.ValueString, String: "gateway"}},
		Limit: 10,
	}
	if rows := run(q); len(rows) != 2 {
		t.Fatalf("service filter: got %d rows, want 2", len(rows))
	}
	rows, err := client.Execute(compiler.CompileGetByID(1))
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if len(rows) != 1 || rows[0].ServiceName != "api-gateway" {
		t.Fatalf("get by id: unexpected rows %+v", rows)
	}
}

// FTS is trigger-maintained and 6 columns wide; the explorer's bare
// `logs_fts MATCH ?` + `JOIN logs.id = logs_fts.rowid` + bm25 ranking must
// work unchanged.
func TestCollectorCompat_FTS(t *testing.T) {
	client := openCollectorRO(t, setupCollectorDB(t))

	var ftsRows int
	if err := client.DB().QueryRow(`SELECT count(*) FROM logs_fts`).Scan(&ftsRows); err != nil {
		t.Fatalf("fts count: %v", err)
	}
	if ftsRows != 3 {
		t.Fatalf("trigger-maintained FTS: got %d rows, want 3", ftsRows)
	}

	q := &dsl.Query{Where: dsl.MatchExpr{Query: "timeout"}, Limit: 10}
	if err := dsl.Validate(q); err != nil {
		t.Fatalf("validate: %v", err)
	}
	dsl.Normalize(q)
	cq, err := compiler.Compile(q)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !strings.Contains(cq.SQL, "JOIN logs_fts ON logs.id = logs_fts.rowid") {
		t.Errorf("expected FTS rowid JOIN, got: %s", cq.SQL)
	}
	rows, err := client.Execute(cq)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(rows) != 1 || !strings.Contains(rows[0].Body, "timeout") {
		t.Fatalf("FTS match: unexpected rows %+v", rows)
	}
}
