# Log Explorer

A read-only web UI for querying OpenTelemetry logs stored in SQLite. It pairs
with the [otel-sqlite](https://codeberg.org/nicknad/otel-sqlite) collector but works with any SQLite
database that exposes a `logs` view/table with the columns below.

## Features

- **Full-text search (FTS5)** — tokenized, relevance-ranked search over log
  body and service name via a contentless FTS5 index, combined with structured
  filters in a single query (hybrid search). Falls back to substring match on
  databases without the index.
- **Read-only by design** — the SQLite connection opens in `mode=ro` with
  `PRAGMA query_only = ON`. The FTS index is built by a separate maintenance
  tool, never the sidecar.
- **Web UI** — dark-themed, HTMX-powered table with filter form, pagination,
  and a per-log detail page showing the full OTel payload (body + attributes).
- **JSON API** — `POST /logs/query` accepts a JSON DSL query and returns JSON
  rows for programmatic use.


## Quick Start

### Prerequisites

- Go 1.25+
- A SQLite database with a `logs` view/table exposing these columns:

  | Column        | Type    | Description                          |
  |---------------|---------|--------------------------------------|
  | `id`          | INTEGER | Unique log event id (row link)       |
  | `timestamp_ns`| INTEGER | Event time, nanoseconds since epoch  |
  | `severity_text`| TEXT   | Severity text (ERROR, WARN, INFO…)   |
  | `severity_number`| INTEGER | Numeric OTel severity              |
  | `service_name`| TEXT    | Emitting service name                |
  | `trace_id`    | BLOB    | Binary trace id                      |
  | `span_id`     | BLOB    | Binary span id                       |
  | `body`        | TEXT    | Log body                             |
  | `attributes_json`| TEXT | Inline event attributes as JSON      |

  For the normalized OTel collector schema (`log_event` + `log_resource` with
  BLOB trace/span ids), create a view:

  ```sql
  CREATE VIEW logs AS
  SELECT le.id AS id,
         le.timestamp_ns AS timestamp_ns,
         le.severity_text AS severity_text,
         le.severity_number AS severity_number,
         lr.service_name AS service_name,
         le.trace_id AS trace_id,
         le.span_id AS span_id,
         le.body AS body,
         le.attributes_json AS attributes_json
  FROM log_event le
  JOIN log_resource lr ON le.resource_id = lr.id;
  ```


### Build & Run

```bash
go build -o bin/log-explorer ./cmd/server
./bin/log-explorer -db /path/to/otel-logs.db -addr :8080
```

Then open <http://localhost:8080/logs>.

### Full-text search setup (FTS5)

The sidecar is read-only, so the FTS5 index is built by a separate
maintenance tool with write access:

```bash
go build -o bin/migrate-fts ./cmd/migrate-fts
./bin/migrate-fts -db /path/to/otel-logs.db            # create + rebuild index
./bin/migrate-fts -db /path/to/otel-logs.db -stats     # report index stats
./bin/migrate-fts -db /path/to/otel-logs.db -stats -probe "timeout gateway"
```

The index is a contentless FTS5 table (`logs_fts`) over `body` and
`service_name`, with `rowid = logs.id`. No triggers are used; rerun the
rebuild after new ingestion to refresh the index. The sidecar detects the
index at startup and enables `match` queries; without it, `match` falls back
to a `body LIKE '%...%'` substring search.

### Flags

| Flag    | Default | Description                       |
|---------|---------|-----------------------------------|
| `-db`   | —       | Path to SQLite database (required)|
| `-addr` | `:8080` | HTTP listen address               |

## Usage

### Web UI

1. Set any combination of filters (service, severity, trace/span id, body
   text, time window, limit) and hit **Query**.
2. Use **◀ Prev / Next ▶** to page through results.
3. Click a timestamp to open `/logs/{id}` — the full payload view with
   attributes.

Filters live in the URL query string, so reloading the page keeps them.

### JSON API

`POST /logs/query` with `Content-Type: application/json` and
`Accept: application/json`:

```bash
curl -X POST http://localhost:8080/logs/query \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json' \
  -d '{
        "where": {
          "and": [
            {"eq": ["service_name", "api-gateway"]},
            {"contains": ["body", "timeout"]}
          ]
        },
        "since": "1h",
        "limit": 50
      }'
```

Response:

```json
[
  {
    "id": 1234,
    "timestamp": 1782924637435495963,
    "severity": "ERROR",
    "service_name": "api-gateway",
    "trace_id": "0200000000000000f900000000000000",
    "span_id": "7f851e0000000000",
    "body": "connection timeout to upstream"
  }
]
```

### DSL reference

The `where` clause supports a shorthand JSON format. Each node is a single-key
object whose key is the operator.

**Comparison operators** — `{"op": ["field", "value"]}`:

| Op        | Meaning                          |
|-----------|----------------------------------|
| `eq`      | equal                            |
| `ne`      | not equal                        |
| `gt`      | greater than                     |
| `gte`     | greater than or equal            |
| `lt`      | less than                        |
| `lte`     | less than or equal               |
| `contains`| substring match (LIKE %value%)   |

```json
{"eq": ["severity", "ERROR"]}
{"contains": ["body", "timeout"]}
```

**`between`** — `{"between": ["field", min, max]}`:

```json
{"between": ["timestamp", 1700000000000000000, 1700000099000000000]}
```

**`in`** — `{"in": ["field", [v1, v2, ...]]}`:

```json
{"in": ["service_name", ["api-gateway", "auth-svc"]]}
```

**Logical combinators** — `{"and": [expr, expr]}` / `{"or": [expr, expr]}`:

```json
{
  "and": [
    {"eq": ["service_name", "api-gateway"]},
    {"or": [
      {"eq": ["severity", "ERROR"]},
      {"eq": ["severity", "WARN"]}
    ]}
  ]
}
```

**Full-text match** — `{"match": "query string"}` (leaf node, FTS5 syntax):

```json
{"match": "timeout gateway"}
```

The match node uses SQLite FTS5 query syntax (tokens are AND-ed by default;
use `OR` / `NOT` / `*` for advanced queries). It is only valid on databases
with a `logs_fts` index; otherwise it falls back to a body substring match.
Max query length is 256 characters.

A hybrid query combining full-text and structured filters:

```json
{
  "where": {
    "and": [
      {"eq": ["service_name", "api-gateway"]},
      {"match": "timeout OR connection refused"}
    ]
  },
  "since": "1h",
  "limit": 50
}
```

**Top-level fields:**

| Field    | Type     | Description                                  |
|----------|----------|----------------------------------------------|
| `select` | string   | `*` (default) or comma-separated field list  |
| `where`  | expr     | Filter tree (see above)                      |
| `since`  | string   | Duration window, e.g. `"1h"`, `"24h"`, `"7d"`|
| `sort`   | []Sort   | `[{field, desc}]`; defaults to timestamp DESC|
| `limit`  | int      | 1–1000, default 100                          |
| `offset` | int      | Pagination offset, default 0                 |

**Allowed fields:** `id`, `timestamp`, `severity`, `service_name`,
`trace_id`, `span_id`, `body`.


## License

[MIT](LICENSE)
