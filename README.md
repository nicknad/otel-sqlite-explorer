# Log Explorer

A read-only web UI for querying OpenTelemetry logs stored in SQLite. It pairs
with the [otel-sqlite](https://github.com/nicknad/otel-sqlite/pkgs/container/otel-sqlite) collector but works with any SQLite
database that exposes a `logs` view/table with the columns below.

## Features

- **Full-text search (FTS5)** — tokenized, relevance-ranked search over log
  body and service name via a contentless FTS5 index, combined with structured
  filters in a single query (hybrid search). Falls back to substring match on
  databases without the index.
- **Read-only by design** — the SQLite connection opens in `mode=ro` with
  `PRAGMA query_only = ON`. The explorer never writes to the database; the
  `logs_fts` index is maintained by the write sidecar
  ([otel-sqlite](https://github.com/nicknad/otel-sqlite/pkgs/container/otel-sqlite) collector).
- **Web UI** — dark-themed, HTMX-powered table with filter form, pagination,
  and a per-log detail page showing the full OTel payload (body + attributes).
- **JSON API** — `POST /logs/query` accepts a JSON DSL query and returns JSON
  rows for programmatic use.

## Design notes

- [API, database, and query](docs/API-DB-QUERY.md) — how a request crosses the
  API, query and database layers, the lifecycle statechart, and the rules a
  bounded model check found broken (empty-index searches, queue deadlines,
  liveness probes behind queries).
- [Explorer ↔ sink database contract](docs/CONTRACT.md) — the database surface
  the explorer depends on.
- [Coupling audit](docs/COUPLING-AUDIT.md) — where those assumptions leak.

## Quick Start

### Published image

Release builds are published to GHCR with the same pipeline the sink uses
(`.github/workflows/publish.yml`: per-arch native builds, trivy CVE gate,
keyless cosign signing, digest-pinned acceptance), tagged with the
**otel-sqlite** sink version they pair with:

```bash
docker run --rm -p 8080:8080 \
  -v /path/to/otel-logs.db:/data/otel-logs.db:ro \
  ghcr.io/nicknad/otel-sqlite-explorer:0.4.0
```

The image opens the mounted database read-only and listens on `:8080`; the
database must expose the `logs` view described under Prerequisites below.

### Prerequisites

- Go 1.27+
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

  For large databases, index the hot paths (the view JOIN plus the
  timestamp / severity filters and default sort):

  ```sql
  CREATE INDEX idx_log_event_resource ON log_event(resource_id);
  CREATE INDEX idx_log_event_ts ON log_event(timestamp_ns);
  CREATE INDEX idx_log_event_sev ON log_event(severity_number);
  ```


### Build & Run

```bash
go build -o bin/log-explorer ./cmd/server
./bin/log-explorer -db /path/to/otel-logs.db -addr :8080
```

Then open <http://localhost:8080/logs>.

### Full-text search (FTS5)

The explorer never builds the FTS index itself. It detects a `logs_fts`
index at startup:

- **Index present** — the web UI shows the full-text search box and `match`
  queries use FTS5 syntax ranked by relevance (`bm25`).
- **Index absent** — the search box is hidden and the UI shows a note; DSL
  `match` nodes fall back to a `body LIKE '%...%'` substring match, so JSON
  API clients keep working.

The index itself is created and kept current by the write sidecar
([otel-sqlite](https://github.com/nicknad/otel-sqlite/pkgs/container/otel-sqlite) collector). It is
detected once at startup: restart the explorer after the index is created for
the search box to appear.

## Demo data

Two one-command paths produce a populated database for trying the site:

| Command | What it does | Data shape |
|---|---|---|
| `make serve-mock` | `cmd/mockdb` writes the synthetic schema to `demo/logs.db` and serves it | Fast and offline; seeded generator, FTS on |
| `make serve-sink-demo` | `cmd/sinkdemo` pushes real OTLP traffic through the published `otel-sqlite` sink image, harvests `demo/sink-logs.db`, and serves it | The exact production write path; needs Docker |

When to use which: **synthetic** is instant and works without Docker;
**sink-produced** is slower but proves the collector round trip on your
machine — if the site serves that database, ingestion works end to end.

- Both regenerate the database on every run, so timestamps stay recent.
- `make sink-demo` regenerates `demo/sink-logs.db` without serving; volume
  follows `cmd/sinkdemo -rows N`.
- The image-backed contract test uses the same topology: `make test-image`
  (skips without Docker; `OTEL_SQLITE_IMAGE=<ref>` pins a ref;
  `EXPLORER_SINK_HARVEST_DUMP=<dir>` copies the verified database out for
  inspection).

### Flags

| Flag       | Default | Description                        |
|------------|---------|------------------------------------|
| `-db`      | —       | Path to SQLite database (required) |
| `-addr`    | `127.0.0.1:8080` | HTTP listen address (use `:8080` to expose on all interfaces) |
| `-version` | —       | Print version and exit             |

`GET /healthz` returns `{"status":"ok"}` (503 when the database is
unreachable) for container / load-balancer probes. The service has no
authentication: bind it to localhost or put it behind an authenticated
reverse proxy, and terminate TLS at the proxy when crossing networks.

## Usage

### Web UI

1. Set any combination of filters (service, severity, trace/span id, body
   text, time window, limit) and hit **Query**.
2. Use **◀ Prev / Next ▶** to page through results.
3. Click a timestamp to open `/logs/{id}` — the full payload view with
   attributes.

Filters live in the URL query string, so reloading the page keeps them.
Notes on filter semantics:

- **Severity** means "this level and above" (`ERROR` also matches `FATAL`).
- **Trace / span id** are hex strings matched case-insensitively.
- **Body / service** are literal substring matches (`%` and `_` are not
  wildcards).
- **From** with a bare date starts at the beginning of that day;
  **To** with a bare date covers through the end of that day.
  `datetime-local` values are interpreted in the server's time zone.
  Dates outside 1677-09-21 to 2262-04-11 (the int64 nanosecond range) are
  rejected.
- Invalid filter values (unknown severity, unparseable date or `since`,
  non-numeric or out-of-range `limit`/`offset`) return `400` with an
  explanation instead of silently querying.

### JSON API

`POST /logs/query` requires `Content-Type: application/json` (request bodies
are capped at 1 MiB). With `Accept: application/json` it returns JSON rows;
without it, it returns an HTML `<tr>` partial for HTMX-style embedding.
Unknown top-level fields are rejected so typos fail loudly. An empty result
set encodes as `[]`.

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
    "severity_number": 17,
    "service_name": "api-gateway",
    "trace_id": "0200000000000000f900000000000000",
    "span_id": "7f851e0000000000",
    "body": "connection timeout to upstream",
    "attributes_json": "{\"component\":\"gateway\",\"attempt\":2}"
  }
]
```

A partial `select` returns zero values for the fields that were not selected
(e.g. `{"select":"body"}` still emits `id: 0` and empty strings), so clients
should rely on the requested field list rather than checking for absence.

### DSL reference

The `where` clause supports a shorthand JSON format. Each node is a single-key
object whose key is the operator.

**Comparison operators** — `{"op": ["field", value]}`. The value may be a
JSON string, number, or boolean (`{"eq": ["severity_number", 17]}`); numeric
strings stay strings for `contains` (`{"contains": ["body", "404"]}`):

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

**`in`** — `{"in": ["field", [v1, v2, …]]}` (at most 100 values, all of one
type):

```json
{"in": ["service_name", ["api-gateway", "auth-svc"]]}
```

**Logical combinators** — `{"and": [expr, expr, …]}` / `{"or": [expr, …]}`
accept two or more sub-expressions:

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

| Field    | Type     | Description                                               |
|----------|----------|-----------------------------------------------------------|
| `select` | string   | `*` (default) or comma-separated field list               |
| `where`  | expr     | Filter tree (see above)                                   |
| `since`  | string   | Duration window: `"1h"`, `"24h"`, `"7d"`, `"1w"` (positive, `d` = days, `w` = weeks) |
| `sort`   | []Sort   | `[{field, desc}]`; defaults to timestamp DESC (or bm25 relevance for `match` queries) |
| `limit`  | int      | 1–1000, default 100 (`0` also means default)              |
| `offset` | int      | Pagination offset, default 0                              |

**Allowed fields:** `id`, `timestamp`, `severity`, `severity_number`,
`service_name`, `trace_id`, `span_id`, `body`, `attributes_json`.

`trace_id` and `span_id` are assumed to be SQLite BLOB columns (as in the
collector schema and the example view above); they are returned as lowercase
hex and compared as raw bytes.


## License

[MIT](LICENSE)
