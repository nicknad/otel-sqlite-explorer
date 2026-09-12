package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"log-explorer/internal/compiler"
)

// TestExecuteExpiredDeadlineFailsFast verifies that a query whose deadline
// has already passed is rejected without touching the database, so a
// zero/negative configured timeout cannot hang or panic the handler.
func TestExecuteExpiredDeadlineFailsFast(t *testing.T) {
	c := &Client{timeout: -time.Second}
	// The WithTimeout timer fires asynchronously; wait for it to elapse so
	// the fail-fast path is what rejects the query.
	time.Sleep(2 * time.Millisecond)

	_, err := c.Execute(&compiler.CompiledQuery{SQL: "SELECT 1"})
	if err == nil {
		t.Fatal("expected error for already-expired query deadline")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestDefaultQueryTimeoutIsPositive guards the production default so a
// regression cannot silently disable the query deadline.
func TestDefaultQueryTimeoutIsPositive(t *testing.T) {
	if DefaultQueryTimeout <= 0 {
		t.Errorf("DefaultQueryTimeout must be positive, got %v", DefaultQueryTimeout)
	}
}

// TestOpenRejectsDatabaseWithoutLogs verifies a valid SQLite file that lacks
// the `logs` read model fails at startup instead of serving 500s.
func TestOpenRejectsDatabaseWithoutLogs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, execErr := conn.Exec("CREATE TABLE foo (x INTEGER)"); execErr != nil {
		t.Fatalf("create table: %v", execErr)
	}
	_ = conn.Close()

	client, err := Open(path)
	if err == nil {
		_ = client.Close()
		t.Fatal("expected Open to reject a database without the logs read model")
	}
	if !strings.Contains(err.Error(), "logs") {
		t.Errorf("error should mention the logs read model: %v", err)
	}
}

// TestOpenRejectsIncompleteLogsReadModel verifies the startup check names the
// missing columns so schema drift is obvious.
func TestOpenRejectsIncompleteLogsReadModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, execErr := conn.Exec(`CREATE TABLE logs (
		id INTEGER,
		timestamp_ns INTEGER,
		severity_text TEXT,
		severity_number INTEGER,
		service_name TEXT,
		trace_id BLOB,
		span_id BLOB,
		body TEXT
	)`); execErr != nil {
		t.Fatalf("create partial logs table: %v", execErr)
	}
	_ = conn.Close()

	client, err := Open(path)
	if err == nil {
		_ = client.Close()
		t.Fatal("expected Open to reject an incomplete logs read model")
	}
	if !strings.Contains(err.Error(), "attributes_json") {
		t.Errorf("error should name the missing column: %v", err)
	}
}

// TestOpenPathWithURISpecialCharacters verifies a path containing characters
// reserved in SQLite URIs ('#', spaces) opens the intended file, stays
// read-only, and does not create a file at the truncated path.
func TestOpenPathWithURISpecialCharacters(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.db")

	conn, err := sql.Open("sqlite", base)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, execErr := conn.Exec(`CREATE TABLE logs (
		id INTEGER,
		timestamp_ns INTEGER,
		severity_text TEXT,
		severity_number INTEGER,
		service_name TEXT,
		trace_id BLOB,
		span_id BLOB,
		body TEXT,
		attributes_json TEXT
	)`); execErr != nil {
		t.Fatalf("create logs table: %v", execErr)
	}
	if _, execErr := conn.Exec(
		`INSERT INTO logs VALUES (1, 1, 'ERROR', 17, 'svc', NULL, 'boom', '{}')`,
	); execErr != nil {
		t.Fatalf("insert row: %v", execErr)
	}
	_ = conn.Close()

	weird := filepath.Join(dir, "we#ird name.db")
	if renameErr := os.Rename(base, weird); renameErr != nil {
		t.Fatalf("rename to special path: %v", renameErr)
	}

	client, err := Open(weird)
	if err != nil {
		t.Fatalf("open special path: %v", err)
	}
	defer func() { _ = client.Close() }()

	rows, err := client.Execute(compiler.CompileGetByID(1))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(rows) != 1 || rows[0].Body != "boom" {
		t.Fatalf("unexpected rows: %+v", rows)
	}

	// The old DSN parser truncated at '#', creating a stray "we" database.
	if _, statErr := os.Stat(filepath.Join(dir, "we")); !os.IsNotExist(statErr) {
		t.Errorf("a truncated database file was created next to the real one")
	}

	// mode=ro must still be in force for the escaped path.
	if _, execErr := client.DB().Exec("CREATE TABLE should_fail (x INTEGER)"); execErr == nil {
		t.Error("expected write to be rejected on a read-only connection")
	}
}
