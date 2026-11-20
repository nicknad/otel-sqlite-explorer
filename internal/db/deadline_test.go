package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"log-explorer/internal/compiler"

	_ "modernc.org/sqlite"
)

// seedDeadlineDB creates a minimal valid read model (the nine floor columns).
func seedDeadlineDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "logs.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Exec(`CREATE TABLE logs (
		id INTEGER,
		timestamp_ns INTEGER,
		severity_text TEXT,
		severity_number INTEGER,
		service_name TEXT,
		trace_id BLOB,
		span_id BLOB,
		body TEXT,
		attributes_json TEXT
	)`); err != nil {
		t.Fatalf("create logs: %v", err)
	}
	return path
}

// TestExecuteDeadlineCountsTheQueue reproduces R4: a request queued behind a
// busy pool must fail within its own context budget. Before the fix the
// admission wait took no context, and a queued request could return long past
// its budget with a 200.
func TestExecuteDeadlineCountsTheQueue(t *testing.T) {
	path := seedDeadlineDB(t)
	client, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = client.Close() }()

	// Hold every pooled connection so the query must queue for admission.
	conns := make([]*sql.Conn, maxConcurrentReads)
	ctx0 := context.Background()
	for i := range conns {
		c, cerr := client.db.Conn(ctx0)
		if cerr != nil {
			t.Fatalf("grab pool conn %d: %v", i, cerr)
		}
		conns[i] = c
		defer func() { _ = c.Close() }()
	}

	ctx, cancel := context.WithTimeout(ctx0, 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err = client.ExecuteContext(ctx, compiler.CompileGetByID(1))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected the queued query to fail its deadline")
	}
	if elapsed > 3*time.Second {
		t.Errorf("queued query waited %v; the caller's budget must count the pool wait (R4)", elapsed)
	}
}

// TestExecuteBoundedUnderExclusiveLock pins the driver-level bound: a query
// that holds a connection while the sink's writer holds an exclusive lock
// fails at busy_timeout (5 s), never the unbounded waits of the old
// single-mutex design.
func TestExecuteBoundedUnderExclusiveLock(t *testing.T) {
	path := seedDeadlineDB(t)
	client, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = client.Close() }()

	lock, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open lock conn: %v", err)
	}
	defer func() { _ = lock.Close() }()
	if _, err := lock.Exec("BEGIN EXCLUSIVE"); err != nil {
		t.Fatalf("begin exclusive: %v", err)
	}
	defer func() { _, _ = lock.Exec("ROLLBACK") }()

	ctx, cancel := context.WithTimeout(context.Background(), DefaultQueryTimeout)
	defer cancel()
	start := time.Now()
	_, err = client.ExecuteContext(ctx, compiler.CompileGetByID(1))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected the blocked query to fail")
	}
	// busy_timeout (5 s) bounds the wait, well inside the default budget.
	if elapsed > 7*time.Second {
		t.Errorf("blocked query waited %v; busy_timeout must bound it", elapsed)
	}
}

// TestPingOwnLaneWhilePoolBusy reproduces R5: with every pooled connection
// held, Ping must still answer within its own budget via the probe lane.
func TestPingOwnLaneWhilePoolBusy(t *testing.T) {
	path := seedDeadlineDB(t)
	client, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = client.Close() }()

	// Hold every pooled connection so any pool-bound call would queue.
	conns := make([]*sql.Conn, maxConcurrentReads)
	ctx := context.Background()
	for i := range conns {
		c, cerr := client.db.Conn(ctx)
		if cerr != nil {
			t.Fatalf("grab pool conn %d: %v", i, cerr)
		}
		conns[i] = c
		defer func() { _ = c.Close() }()
	}

	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := client.Ping(pingCtx); err != nil {
		t.Fatalf("ping must answer on its own lane: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("ping took %v; it must not queue behind the pool (R5)", d)
	}
}
