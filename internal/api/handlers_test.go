package api

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"log-explorer/internal/db"
	"log-explorer/internal/dsl"
	"log-explorer/internal/schema"

	_ "modernc.org/sqlite"
)

// seedTestDB creates a temp database with the production schema and rows
// log events.
func seedTestDB(t *testing.T, rows int) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "logs.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, execErr := conn.Exec(schema.DDL); execErr != nil {
		t.Fatalf("schema: %v", execErr)
	}
	if _, execErr := conn.Exec(
		`INSERT INTO log_resource (id, service_name) VALUES ('r1', 'svc')`,
	); execErr != nil {
		t.Fatalf("resource: %v", execErr)
	}

	tx, err := conn.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(
		`INSERT INTO log_event
			(timestamp_ns, observed_timestamp_ns, resource_id, severity_number, severity_text, body)
			VALUES (?, ?, 'r1', 9, 'INFO', 'hello')`,
	)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for i := range rows {
		if _, err := stmt.Exec(int64(i)+1, int64(i)+1); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return path
}

// TestRunQueryDetectsNextPageAtMaxLimit is the regression test for the
// pagination probe: re-normalizing limit+1 clamped it back to MaxLimit, so
// the Next button was always disabled at limit=1000 even with more rows.
func TestRunQueryDetectsNextPageAtMaxLimit(t *testing.T) {
	client, err := db.Open(seedTestDB(t, dsl.MaxLimit+1))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	srv, err := NewServer(client)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	pd := pageData{Limit: dsl.MaxLimit, Offset: 0}
	if err := srv.runQuery(context.Background(), &pd); err != nil {
		t.Fatalf("runQuery: %v", err)
	}
	if !pd.HasNext {
		t.Error("expected HasNext=true when more rows exist than MaxLimit")
	}
	if len(pd.Logs) != dsl.MaxLimit {
		t.Errorf("expected %d rows, got %d", dsl.MaxLimit, len(pd.Logs))
	}
	if pd.HasPrev {
		t.Error("expected HasPrev=false on the first page")
	}
}
