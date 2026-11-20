package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"log-explorer/internal/schema"

	_ "modernc.org/sqlite"
)

func TestClassifyFTS(t *testing.T) {
	cases := []struct {
		name       string
		probe      ftsProbe
		wantActive bool
		wantState  ftsState
	}{
		{"absent table", ftsProbe{createSQL: "", maintTriggers: 0}, false, ftsMissing},
		{"plain table named logs_fts", ftsProbe{createSQL: "CREATE TABLE logs_fts (x TEXT)", maintTriggers: 1}, false, ftsMissing},
		{"fts5 without triggers (sink off shape)", ftsProbe{createSQL: "CREATE VIRTUAL TABLE logs_fts USING fts5(body, content='')", maintTriggers: 0}, false, ftsUnmaintained},
		{"backfilled fts5 without triggers", ftsProbe{createSQL: "CREATE VIRTUAL TABLE logs_fts USING fts5(body, service_name, content='')", maintTriggers: 0}, false, ftsUnmaintained},
		{"maintained fts5", ftsProbe{createSQL: "CREATE VIRTUAL TABLE logs_fts USING fts5(body, tokenize='unicode61 remove_diacritics 2', content='', contentless_delete=1)", maintTriggers: 2}, true, ftsActive},
		{"maintained by a single trigger", ftsProbe{createSQL: "CREATE VIRTUAL TABLE logs_fts USING fts5(body)", maintTriggers: 1}, true, ftsActive},
		{"uppercase FTS5 declaration", ftsProbe{createSQL: "CREATE  VIRTUAL  TABLE  logs_fts  USING  FTS5(body)", maintTriggers: 1}, true, ftsActive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyFTS(tc.probe)
			if got.state != tc.wantState {
				t.Fatalf("state = %v, want %v (reason %q)", got.state, tc.wantState, got.reason)
			}
			if got.Active() != tc.wantActive {
				t.Fatalf("Active() = %v, want %v", got.Active(), tc.wantActive)
			}
		})
	}
}

func TestIsFts5VirtualTable(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"CREATE VIRTUAL TABLE logs_fts USING fts5(body)", true},
		{"CREATE VIRTUAL TABLE logs_fts USING FTS5(body, x)", true},
		{"CREATE TABLE logs_fts (body TEXT)", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isFts5VirtualTable(tc.in); got != tc.want {
			t.Errorf("isFts5VirtualTable(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// readModelDDL is the minimal read-model floor the explorer requires: a
// logs table exposing every requiredLogColumns column.
const readModelDDL = "CREATE TABLE logs (id INTEGER, timestamp_ns INTEGER, severity_text TEXT, severity_number INTEGER, service_name TEXT, trace_id BLOB, span_id BLOB, body TEXT, attributes_json TEXT);"

// detectionDB builds a temp database from DDL statements and opens it
// through the production read-only path.
func detectionDB(t *testing.T, ddl ...string) (*Client, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "detect.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, stmt := range ddl {
		if _, err := conn.Exec(stmt); err != nil {
			_ = conn.Close()
			t.Fatalf("exec: %v", err)
		}
	}
	_ = conn.Close()
	client, err := Open(path)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	return client, func() { _ = client.Close() }
}

func TestDetectOffModeShapeIsInactive(t *testing.T) {
	// The bug: the sink's search mode 'off' leaves an EMPTY logs_fts table
	// with NO maintenance triggers. Table existence alone must not
	// advertise full-text search.
	client, done := detectionDB(t,
		readModelDDL,
		"CREATE VIRTUAL TABLE logs_fts USING fts5(body, content='', contentless_delete=1)",
	)
	defer done()
	if client.HasFTS() {
		t.Fatal("expected HasFTS=false for an unmaintained (off-mode shape) logs_fts")
	}
}

func TestDetectBackfilledWithoutTriggersIsInactive(t *testing.T) {
	// The old fixture/mockdb shape: one-shot backfill, no triggers.
	client, done := detectionDB(t,
		readModelDDL,
		"CREATE VIRTUAL TABLE logs_fts USING fts5(body, service_name, content='')",
		"INSERT INTO logs_fts(rowid, body, service_name) SELECT id, body, service_name FROM logs",
	)
	defer done()
	if client.HasFTS() {
		t.Fatal("expected HasFTS=false for a backfilled, unmaintained index")
	}
}

func TestDetectPlainTableNamedLogsFtsIsInactive(t *testing.T) {
	client, done := detectionDB(t,
		readModelDDL,
		"CREATE TABLE logs_fts (body TEXT)",
	)
	defer done()
	if client.HasFTS() {
		t.Fatal("expected HasFTS=false for a plain table named logs_fts")
	}
}

func TestDetectMaintainedIndexIsActive(t *testing.T) {
	// The sink's default body-mode shape: an fts5 table plus the producer's
	// trigger pair from internal/schema.FTSTriggers.
	client, done := detectionDB(t,
		readModelDDL,
		"CREATE TABLE log_event (id INTEGER PRIMARY KEY, body TEXT)",
		"CREATE VIRTUAL TABLE logs_fts USING fts5(body, content='', contentless_delete=1)",
		schema.FTSTriggers,
	)
	defer done()
	if !client.HasFTS() {
		t.Fatal("expected HasFTS=true for a trigger-maintained index")
	}
}
