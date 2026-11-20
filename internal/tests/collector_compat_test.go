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

// collectorDDL mirrors the current otel-sqlite sink baseline (006, squashed
// logs-only), regenerated 2026-11-19 from the database produced by
// ghcr.io/nicknad/otel-sqlite:latest (image digest sha256:f8e5bbe2f7fa, see
// internal/tests/sink_image_test.go): a 20-column superset logs view, a
// body-only contentless logs_fts with (rowid, body) maintenance triggers,
// stamped migrations 000-006, and search_index_state at body/ready. The
// explorer only SELECTs its 9-column subset, so this must keep working
// without code changes.
const collectorDDL = `
CREATE TABLE schema_migrations (
    version     TEXT PRIMARY KEY,
    applied_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    description TEXT
);
INSERT INTO schema_migrations (version, description) VALUES
    ('000','Satisfied by squashed logs-only baseline'),
    ('001','Satisfied by squashed logs-only baseline'),
    ('002','Satisfied by squashed logs-only baseline'),
    ('003','Satisfied by squashed logs-only baseline'),
    ('004','Satisfied by squashed logs-only baseline'),
    ('005','Satisfied by squashed logs-only baseline'),
    ('006','Satisfied by squashed logs-only baseline');
CREATE TABLE log_resource (
    id              TEXT PRIMARY KEY,
    service_name    TEXT NOT NULL,
    host_name       TEXT,
    schema_url      TEXT,
    attributes_json TEXT NOT NULL DEFAULT '{}',
    created_at      TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_log_resource_service ON log_resource(service_name);
CREATE TABLE log_event (
    id                       INTEGER PRIMARY KEY,
    resource_id              TEXT NOT NULL,
    timestamp_ns             INTEGER NOT NULL,
    observed_timestamp_ns    INTEGER NOT NULL,
    severity_number          INTEGER NOT NULL,
    severity_text            TEXT,
    trace_id                 BLOB,
    span_id                  BLOB,
    body                     TEXT,
    event_name               TEXT,
    flags                    INTEGER NOT NULL DEFAULT 0,
    dropped_attributes_count INTEGER NOT NULL DEFAULT 0,
    scope_name               TEXT,
    scope_version            TEXT,
    attributes_json          TEXT NOT NULL DEFAULT '{}',
    scope_attributes_json    TEXT NOT NULL DEFAULT '{}',
    scope_schema_url         TEXT,
    FOREIGN KEY (resource_id)
        REFERENCES log_resource(id)
        ON DELETE CASCADE
);
CREATE INDEX idx_log_event_timestamp ON log_event(timestamp_ns);
CREATE INDEX idx_log_event_resource_timestamp
    ON log_event(resource_id, timestamp_ns);
CREATE INDEX idx_log_event_trace_timestamp
    ON log_event(trace_id, timestamp_ns)
    WHERE trace_id IS NOT NULL;
CREATE VIEW logs AS
SELECT
    le.id                       AS id,
    le.resource_id              AS resource_id,
    le.timestamp_ns             AS timestamp_ns,
    le.observed_timestamp_ns    AS observed_timestamp_ns,
    le.severity_number          AS severity_number,
    le.severity_text            AS severity_text,
    le.trace_id                 AS trace_id,
    le.span_id                  AS span_id,
    le.body                     AS body,
    le.event_name               AS event_name,
    le.flags                    AS flags,
    le.dropped_attributes_count AS dropped_attributes_count,
    le.scope_name               AS scope_name,
    le.scope_version            AS scope_version,
    le.scope_attributes_json    AS scope_attributes_json,
    le.scope_schema_url         AS scope_schema_url,
    le.attributes_json          AS attributes_json,
    lr.service_name             AS service_name,
    lr.host_name                AS host_name,
    lr.schema_url               AS resource_schema_url
FROM log_event AS le
JOIN log_resource AS lr
    ON lr.id = le.resource_id;
CREATE VIRTUAL TABLE logs_fts USING fts5(
    body,
    tokenize = 'unicode61 remove_diacritics 2',
    content = '',
    contentless_delete = 1
);
CREATE TRIGGER logs_fts_ai AFTER INSERT ON log_event BEGIN
    INSERT INTO logs_fts (rowid, body)
    VALUES (new.id, COALESCE(new.body, ''));
END;
CREATE TRIGGER logs_fts_ad AFTER DELETE ON log_event BEGIN
    DELETE FROM logs_fts WHERE rowid = old.id;
END;
CREATE TABLE search_index_state (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    mode          TEXT    NOT NULL CHECK (mode IN ('off', 'body', 'full')),
    status        TEXT    NOT NULL CHECK (status IN ('ready', 'rebuilding')),
    last_rowid    INTEGER NOT NULL DEFAULT 0,
    rebuild_until INTEGER,
    updated_at    TEXT    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (
        (mode = 'off'
            AND status = 'ready'
            AND last_rowid = 0
            AND rebuild_until IS NULL)
        OR
        (mode <> 'off'
            AND (
                (status = 'ready' AND rebuild_until IS NULL)
                OR
                (status = 'rebuilding'
                    AND rebuild_until IS NOT NULL
                    AND rebuild_until >= last_rowid)
            ))
    )
);
INSERT INTO search_index_state (id, mode, status, last_rowid) VALUES (1, 'body', 'ready', 0);
`

// setupCollectorDB builds a temp DB using the sink's 006 DDL (not the
// explorer's synthetic demo schema), populates it via the sink's triggers
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
	// The 006 sink also stamps schema_migrations and search_index_state;
	// the explorer must ignore everything it does not query.
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

// FTS is trigger-maintained and body-only (the 006 default mode); the
// explorer's bare
// `logs_fts MATCH ?` + `JOIN logs.id = logs_fts.rowid` + bm25 ranking must
// work unchanged.
func TestCollectorCompat_FTS(t *testing.T) {
	path := setupCollectorDB(t)
	client := openCollectorRO(t, path)

	// Fixture introspection opens its own connection: it verifies the
	// harvested schema, not db.Client's read path.
	iconn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture for introspection: %v", err)
	}
	defer func() { _ = iconn.Close() }()
	var ftsRows int
	if err := iconn.QueryRow(`SELECT count(*) FROM logs_fts`).Scan(&ftsRows); err != nil {
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
