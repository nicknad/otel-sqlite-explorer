// Package schema provides the synthetic demo schema used by cmd/mockdb and
// the plain test fixtures: a minimal OTel-flavored layout (log_resource +
// log_event) plus a 9-column `logs` view that satisfies the explorer's
// read-model floor (requiredLogColumns in internal/db).
//
// This is NOT the otel-sqlite sink's schema. The live sink installs a
// 20-column superset `logs` view, schema_migrations, search_index_state and
// a body-only FTS5 index; that contract is documented in docs/CONTRACT.md
// and verified against the real image by internal/tests/sink_image_test.go.
package schema

// DDL creates the synthetic demo schema (mockdb + plain fixtures). It is
// safe to Exec as-is.
const DDL = `
CREATE TABLE log_resource (
	id TEXT PRIMARY KEY,
	service_name TEXT NOT NULL,
	host_name TEXT,
	schema_url TEXT,
	attributes_json TEXT
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
	event_name TEXT,
	flags INTEGER NOT NULL DEFAULT 0,
	dropped_attributes_count INTEGER NOT NULL DEFAULT 0,
	scope_name TEXT,
	scope_version TEXT,
	attributes_json TEXT NOT NULL DEFAULT '{}',
	FOREIGN KEY (resource_id) REFERENCES log_resource(id)
);

CREATE VIEW logs AS
SELECT
	le.id                AS id,
	le.timestamp_ns      AS timestamp_ns,
	le.severity_text     AS severity_text,
	le.severity_number   AS severity_number,
	le.trace_id          AS trace_id,
	le.span_id           AS span_id,
	le.body              AS body,
	le.attributes_json   AS attributes_json,
	lr.service_name      AS service_name
FROM log_event le
JOIN log_resource lr ON le.resource_id = lr.id;

-- Indexes approximating the explorer's hot paths for the synthetic demo
-- schema. The live sink's index set differs (no severity index; different
-- names and shapes for the others) - see docs/CONTRACT.md and the
-- sink_image_test harvest for the real shapes.
CREATE INDEX IF NOT EXISTS idx_log_event_resource ON log_event(resource_id);
CREATE INDEX IF NOT EXISTS idx_log_event_ts ON log_event(timestamp_ns);
CREATE INDEX IF NOT EXISTS idx_log_event_sev ON log_event(severity_number);
`
