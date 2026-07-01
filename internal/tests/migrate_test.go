package tests

import (
	"database/sql"
	"os"
	"testing"

	_ "modernc.org/sqlite"

	"log-explorer/internal/db"
	"log-explorer/internal/migrate"
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
	f.Close()

	conn, err := sql.Open("sqlite", path)
	if err != nil {
		os.Remove(path)
		t.Fatalf("open: %v", err)
	}
	if _, err := conn.Exec(`CREATE TABLE logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp INTEGER,
		severity TEXT,
		service_name TEXT,
		trace_id TEXT,
		span_id TEXT,
		body TEXT
	)`); err != nil {
		conn.Close()
		os.Remove(path)
		t.Fatalf("create logs: %v", err)
	}
	rows := []struct {
		ts, sev, svc, body string
	}{
		{"1", "ERROR", "api-gateway", "connection timeout to upstream"},
		{"2", "INFO", "api-gateway", "request completed"},
		{"3", "WARN", "auth-svc", "rate limit approaching"},
	}
	for _, r := range rows {
		if _, err := conn.Exec(
			"INSERT INTO logs (timestamp, severity, service_name, body) VALUES (?,?,?,?)",
			r.ts, r.sev, r.svc, r.body,
		); err != nil {
			conn.Close()
			os.Remove(path)
			t.Fatalf("insert: %v", err)
		}
	}
	return path, conn
}

func TestMigrateRebuildCreatesIndex(t *testing.T) {
	path, conn := setupPlainDB(t)
	defer conn.Close()
	defer os.Remove(path)

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
	defer conn.Close()
	defer os.Remove(path)

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
	defer conn.Close()
	defer os.Remove(path)

	if _, err := migrate.RebuildFTS(conn); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	// Insert a new log with a distinctive token WITHOUT touching the FTS
	// index (no triggers exist). Its token must NOT be findable via MATCH.
	if _, err := conn.Exec(
		"INSERT INTO logs (timestamp, severity, service_name, body) VALUES (?,?,?,?)",
		"4", "ERROR", "api-gateway", "new zzz_drift_token error",
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
	if _, err := migrate.RebuildFTS(conn); err != nil {
		t.Fatalf("rebuild resync: %v", err)
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
	defer conn.Close()
	defer os.Remove(path)

	if _, err := migrate.RebuildFTS(conn); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	conn.Close()

	// Open read-only via the production db package and confirm a token
	// search finds the right row.
	client, err := db.Open(path)
	if err != nil {
		t.Fatalf("reopen ro: %v", err)
	}
	defer client.Close()
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
