package tests

import (
	"database/sql"
	"os"
	"testing"

	"log-explorer/internal/db"
	"log-explorer/internal/migrate"

	_ "modernc.org/sqlite"
)

// setupPlainDB creates a writable temp DB with a logs table and a few rows,
// returning the path. It intentionally does NOT create an FTS index.
func setupPlainDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	f, err := os.CreateTemp("", "migrate-*.db")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	path := f.Name()
	_ = f.Close()

	conn, err := sql.Open("sqlite", path)
	if err != nil {
		_ = os.Remove(path)
		t.Fatalf("open: %v", err)
	}
	if _, err := conn.Exec(`
		CREATE TABLE log_resource (
			id TEXT PRIMARY KEY,
			service_name TEXT NOT NULL
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
			flags INTEGER NOT NULL DEFAULT 0,
			dropped_attributes_count INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY (resource_id) REFERENCES log_resource(id)
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
		JOIN log_resource lr ON le.resource_id = lr.id;
	`); err != nil {
		_ = conn.Close()
		_ = os.Remove(path)
		t.Fatalf("create schema: %v", err)
	}

	// Insert resources.
	for _, r := range []struct{ id, svc string }{
		{"res-api", "api-gateway"},
		{"res-auth", "auth-svc"},
	} {
		if _, err := conn.Exec(
			"INSERT INTO log_resource (id, service_name) VALUES (?, ?)",
			r.id, r.svc,
		); err != nil {
			_ = conn.Close()
			_ = os.Remove(path)
			t.Fatalf("insert resource: %v", err)
		}
	}

	type row struct {
		ts, sevNum, sev, svc, body, resID string
	}
	for _, r := range []row{
		{"1", "17", "ERROR", "api-gateway", "connection timeout to upstream", "res-api"},
		{"2", "9", "INFO", "api-gateway", "request completed", "res-api"},
		{"3", "13", "WARN", "auth-svc", "rate limit approaching", "res-auth"},
	} {
		if _, err := conn.Exec(
			`INSERT INTO log_event
				(timestamp_ns, observed_timestamp_ns, severity_number, severity_text, body, resource_id)
				VALUES (?, ?, ?, ?, ?, ?)`,
			r.ts, r.ts, r.sevNum, r.sev, r.body, r.resID,
		); err != nil {
			_ = conn.Close()
			_ = os.Remove(path)
			t.Fatalf("insert: %v", err)
		}
	}
	return path, conn
}

func TestMigrateRebuildCreatesIndex(t *testing.T) {
	path, conn := setupPlainDB(t)
	defer func() { _ = conn.Close() }()
	defer func() { _ = os.Remove(path) }()

	// Before: no FTS table.
	s, err := migrate.Stats(conn)
	if err != nil {
		t.Fatalf("stats before: %v", err)
	}
	if s.HasFTS {
		t.Fatal("expected no fts table before rebuild")
	}

	// Rebuild creates the table and populates it.
	s, err = migrate.RebuildFTS(conn)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if !s.HasFTS {
		t.Fatal("expected fts table after rebuild")
	}
	if s.LogsCount != 3 || s.FTSCount != 3 {
		t.Fatalf("expected 3/3 rows, got logs=%d fts=%d", s.LogsCount, s.FTSCount)
	}
}

func TestMigrateRebuildIsIdempotent(t *testing.T) {
	path, conn := setupPlainDB(t)
	defer func() { _ = conn.Close() }()
	defer func() { _ = os.Remove(path) }()

	if _, err := migrate.RebuildFTS(conn); err != nil {
		t.Fatalf("first rebuild: %v", err)
	}
	// Second rebuild must not error and must leave counts stable.
	s, err := migrate.RebuildFTS(conn)
	if err != nil {
		t.Fatalf("second rebuild: %v", err)
	}
	if s.FTSCount != 3 {
		t.Fatalf("idempotent rebuild failed: fts=%d", s.FTSCount)
	}
}

func TestMigrateRebuildDetectsDriftAfterInsert(t *testing.T) {
	path, conn := setupPlainDB(t)
	defer func() { _ = conn.Close() }()
	defer func() { _ = os.Remove(path) }()

	if _, err := migrate.RebuildFTS(conn); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	// Insert a new log with a distinctive token WITHOUT touching the FTS
	// index (no triggers exist). Insert into log_event, not the logs VIEW.
	if _, err := conn.Exec(
		`INSERT INTO log_event
			(timestamp_ns, observed_timestamp_ns, severity_number, severity_text, body, resource_id)
			VALUES (?, ?, ?, ?, ?, ?)`,
		"4", "4", "17", "ERROR", "new zzz_drift_token error", "res-api",
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	n, err := migrate.ProbeToken(conn, "zzz_drift_token")
	if err != nil {
		t.Fatalf("probe before rebuild: %v", err)
	}
	if n != 0 {
		t.Fatalf("ungated insert token should not be indexed, got %d matches", n)
	}

	// Rebuild resyncs and the token becomes findable.
	if _, rebuildErr := migrate.RebuildFTS(conn); rebuildErr != nil {
		t.Fatalf("rebuild resync: %v", rebuildErr)
	}
	n, err = migrate.ProbeToken(conn, "zzz_drift_token")
	if err != nil {
		t.Fatalf("probe after rebuild: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 match after rebuild, got %d", n)
	}
}

func TestMigrateRebuildMakesMatchQueriesWork(t *testing.T) {
	path, conn := setupPlainDB(t)
	defer func() { _ = conn.Close() }()
	defer func() { _ = os.Remove(path) }()

	if _, err := migrate.RebuildFTS(conn); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	_ = conn.Close()

	// Open read-only via the production db package and confirm a token
	// search finds the right row.
	client, err := db.Open(path)
	if err != nil {
		t.Fatalf("reopen ro: %v", err)
	}
	defer func() { _ = client.Close() }()
	if !client.HasFTS() {
		t.Fatal("read-only client should detect fts table")
	}
	var body string
	err = client.DB().QueryRow(
		"SELECT logs.body FROM logs JOIN logs_fts ON logs.id = logs_fts.rowid WHERE logs_fts MATCH ?",
		"timeout",
	).Scan(&body)
	if err != nil {
		t.Fatalf("match query: %v", err)
	}
	if body != "connection timeout to upstream" {
		t.Fatalf("unexpected body: %s", body)
	}
}
