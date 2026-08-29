// Package schema provides the production DDL for the log-explorer SQLite
// database: the normalized OTel collector layout (log_resource + log_event)
// plus the read model's `logs` view. The mock generator and the integration
// tests share it so both always exercise the real schema.
package schema

// DDL creates the production schema. It is safe to Exec as-is.
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
`
