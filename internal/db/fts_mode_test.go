package db

import (
	"testing"

	"log-explorer/internal/schema"
)

func TestSearchModeMirrorsStateRow(t *testing.T) {
	client, done := detectionDB(t,
		readModelDDL,
		"CREATE TABLE log_event (id INTEGER PRIMARY KEY, body TEXT)",
		"CREATE VIRTUAL TABLE logs_fts USING fts5(body, content='', contentless_delete=1)",
		schema.FTSTriggers,
		schema.SearchStateFull,
	)
	defer done()
	if !client.HasFTS() {
		t.Fatal("expected an active maintained index")
	}
	if got := client.SearchMode(); got != "full" {
		t.Fatalf("SearchMode() = %q, want full (the state row declares it)", got)
	}
}

func TestSearchModeBodyDefault(t *testing.T) {
	client, done := detectionDB(t,
		readModelDDL,
		"CREATE TABLE log_event (id INTEGER PRIMARY KEY, body TEXT)",
		"CREATE VIRTUAL TABLE logs_fts USING fts5(body, content='', contentless_delete=1)",
		schema.FTSTriggers,
		schema.SearchStateBody,
	)
	defer done()
	if got := client.SearchMode(); got != "body" {
		t.Fatalf("SearchMode() = %q, want body (the sink's 006 default)", got)
	}
}

func TestSearchModeEmptyWithoutStateRow(t *testing.T) {
	client, done := detectionDB(t, readModelDDL)
	defer done()
	if got := client.SearchMode(); got != "" {
		t.Fatalf("SearchMode() = %q, want empty on foreign databases", got)
	}
}

func TestSearchModeDoesNotAffectClassification(t *testing.T) {
	// The state row may declare a mode while the index is unmaintained (the
	// sink's 'off' shape): mode is advisory copy, never a detection input.
	client, done := detectionDB(t,
		readModelDDL,
		"CREATE VIRTUAL TABLE logs_fts USING fts5(body, content='', contentless_delete=1)",
	)
	defer done()
	if client.HasFTS() {
		t.Fatal("an unmaintained index must not be active regardless of any state row")
	}
}
