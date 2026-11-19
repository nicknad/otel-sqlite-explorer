package tests

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"log-explorer/internal/db"

	_ "modernc.org/sqlite"
)

// TestFloorSabotageCanary proves the read-model floor check can fail: it
// rebuilds the `logs` view with one floor column renamed and asserts that
// db.Open rejects the sabotaged database with the missing-column error.
// A floor check that cannot be observed failing verifies nothing, so this
// test is the executable half of CONTRACT.md §7 / COUPLING-AUDIT.md §10.4.
func TestFloorSabotageCanary(t *testing.T) {
	path := setupTestDB(t)
	t.Cleanup(func() { _ = os.Remove(path) })

	// Sabotage: recreate the view with severity_text renamed, leaving the
	// other eight floor columns intact.
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open for sabotage: %v", err)
	}
	if _, err := conn.Exec(`DROP VIEW logs`); err != nil {
		t.Fatalf("drop view: %v", err)
	}
	if _, err := conn.Exec(`
CREATE VIEW logs AS
SELECT
	le.id                AS id,
	le.timestamp_ns      AS timestamp_ns,
	le.severity_text     AS severity_txt,
	le.severity_number   AS severity_number,
	le.trace_id          AS trace_id,
	le.span_id           AS span_id,
	le.body              AS body,
	le.attributes_json   AS attributes_json,
	lr.service_name      AS service_name
FROM log_event le
JOIN log_resource lr ON le.resource_id = lr.id;`); err != nil {
		t.Fatalf("recreate sabotaged view: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close sabotage conn: %v", err)
	}

	client, err := db.Open(path)
	if err == nil {
		_ = client.Close()
		t.Fatal("floor validation: expected db.Open to reject a read model missing severity_text")
	}
	if !strings.Contains(err.Error(), "missing required column") {
		t.Fatalf("floor validation: unexpected error: %v", err)
	}
	t.Logf("floor validation failed as designed: %v", err)
}
