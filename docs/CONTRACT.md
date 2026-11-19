# Explorer ↔ Sink Database Contract

**Status: PROPOSED — not yet adopted.** Decisions marked **[OPEN D-n]** belong to the
maintainer and are listed with options in `docs/COUPLING-AUDIT.md`.

**Parties**

- *Producer:* `otel-sqlite` — Rust OTLP→SQLite sink (github.com/nicknad/otel-sqlite).
  Canonical schema: `crates/otel-sqlite-storage/migrations/baseline_006.sql` for fresh
  databases, equal in effect to the frozen replay chain `migrations/upgrades/000…006`
  (verified by executable diff, 2026-11-19).
  Runtime artifact: the published container image ghcr.io/nicknad/otel-sqlite
  (GitHub package, tag `latest` as of 2026-11-19); the explorer's image-backed
  test (internal/tests/sink_image_test.go) verifies the contract against it.
- *Consumer:* `otel-sqlite-explorer` — Go read-only web UI (this repository).

**Artifact:** the SQLite database file. It is the only coupling surface. Everything
else (network, config, process boundaries) is out of contract.

---

## 1. Versioning and handshake

The sink stamps applied migrations in:

```sql
CREATE TABLE schema_migrations (
    version     TEXT PRIMARY KEY,
    applied_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    description TEXT
);
```

- Stamping is performed by the sink's migration runner at startup; the `.sql` files
  alone do not insert rows. Current schema version: **006**.
- The explorer SHOULD read `SELECT version FROM schema_migrations` at startup
  (it currently does not read it at all):
  - table absent → foreign or pre-canonical database: fall back to the read-model
    floor check alone (§2) and make no sink assumptions;
  - only versions the explorer recognizes (currently `000`–`006`) → fully known DB;
  - any version above the explorer's known set → newer sink than explorer. The
    compatibility policy (**[OPEN D-1]**, §5) decides whether this is a warning, a
    refusal, or silent tolerance.

## 2. Read-model floor (required columns)

The explorer requires a `logs` view or table exposing exactly these nine columns
(mirrored in code as `requiredLogColumns`, `internal/db/client.go`):

| Column            | Type    | Notes                                        |
|-------------------|---------|----------------------------------------------|
| `id`              | INTEGER | Row link into `log_event`                    |
| `timestamp_ns`    | INTEGER | Event time, nanoseconds since epoch          |
| `severity_text`   | TEXT    | Verbatim producer spelling                   |
| `severity_number`  | INTEGER | OTel numeric severity scale                  |
| `service_name`     | TEXT    | From `log_resource` via the view join      |
| `trace_id`         | BLOB    | Binary trace id (compare as BLOB, never text) |
| `span_id`          | BLOB    | Binary span id                               |
| `body`             | TEXT    | Log record body                              |
| `attributes_json`  | TEXT    | Inline attributes as a JSON string           |

The canonical 006 `logs` view is a **20-column superset** (verified 2026-11-19: all
nine floor columns present, order stable). The explorer SELECTs only its nine and
MUST ignore unknown tables, columns and views. This superset tolerance is what has
kept the two repos compatible so far and is normative.

Base tables `log_resource`, `log_event` and their indexes are **informational only**;
the explorer must never query them directly.

## 3. Search index (FTS) capability

FTS availability is a **runtime fact**, not a constant. Since sink migration 006
the index shape is derived from the sink's `[search] mode` and recorded in a
single-row state table:

```sql
CREATE TABLE search_index_state (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    mode          TEXT    NOT NULL CHECK (mode IN ('off', 'body', 'full')),
    status        TEXT    NOT NULL CHECK (status IN ('ready', 'rebuilding')),
    last_rowid    INTEGER NOT NULL DEFAULT 0,
    rebuild_until INTEGER,
    updated_at    TEXT    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    ...cross-field CHECKs...
);
```

Installed shapes:

| Mode   | `logs_fts` shape                                      | Maintenance triggers | Searchable content        |
|--------|---------------------------------------------------------|----------------------|---------------------------|
| `off`    | table present, **empty**                                | none                 | none (fallback required)  |
| `body`  | contentless FTS5(body) — **default** since 006          | `logs_fts_ai`/`logs_fts_ad` | body only                 |
| `full`  | legacy contentless FTS5(body, service_name, host_name, severity_text, event_name, scope_name) | `logs_fts_ai`/`logs_fts_ad` | all six columns           |

Additional runtime facts:

- After a mode change or the 006 upgrade the state row is `status='rebuilding'`:
  new rows are searchable immediately, historical rows appear as the bounded
  background rebuild advances. **Partial results during rebuild are normal, not an
  error.**
- The explorer MUST NOT assume any particular indexed column set: it already emits
  table-level `logs_fts MATCH ?` (shape-agnostic). Keep that invariant.
- UI copy must not overclaim: "words match … body & service" is true only in
  `full` mode; in the default `body` mode only the body is indexed.

### Capability detection **[OPEN D-2]**

Today the explorer detects FTS by table existence (`internal/db/client.go`,
`hasFTS`). In `off` mode the table exists but is empty, so the explorer silently
reports FTS available and returns zero hits — a live bug against any `mode="off"`
database. Candidate mechanisms:

- **(a) Trigger probe:** FTS is active iff `logs_fts` exists AND a maintenance
  trigger referencing it exists in `sqlite_master`. Cheap; works on any DB.
- **(b) State row:** read `search_index_state` (mode + status). Strongest signal,
  available on 006+ sink DBs; also exposes `rebuilding` for correct UX.
- **(c) Row-count probe:** rejected — wrong for rebuilds and empty databases.

Recommended combination (see audit §10): (b) when the table exists, (a) as the
pre-006 / foreign-database fallback. Decision stays open.

### Fallback behavior

When capability is false, `MatchExpr` nodes are rewritten to body substring
matches (existing `rewriteMatchToContains`). This behavior is normative and must
be exercised by tests in every fixture variant, including `off` mode.

## 4. Conventions

- `timestamp_ns`: int64 nanoseconds since epoch; date filters operate on it.
- `trace_id`/`span_id`: raw bytes (BLOB); never compare as text.
- `attributes_json`: JSON string; the explorer renders it, never parses it into SQL.
- The explorer opens the DB `mode=ro` with `PRAGMA query_only = ON` — it never
  writes, builds, or repairs the index. This is normative.

## 5. Compatibility policy **[OPEN D-1]**

| Option | Meaning | Cost |
|--------|---------|------|
| A. Floor-only (status quo, implicit) | Supports any DB with the 9-column `logs` view; sink compat unverified | Zero infra; silent drift (already materialized twice); `off`-mode bug class stays possible |
| B. Verified current + declared floor | Explorer CI proves compatibility against the current sink baseline; floor documented as this contract | One fixture set + one CI job; realistic now (sink has no releases yet) |
| B+. Verified N-1 window | B, plus fixtures from the previous sink release | Requires sink tagging/releases to exist first |
| C. Lockstep | Explorer releases tag-matched to sink | Highest coordination cost; unnecessary while the floor is additive-stable |

The sink's own policy for its binary is "binary at-or-above DB schema" (see its
`docs/operations/upgrades.md`). The explorer, being a reader, is naturally
tolerant; B is the pragmatic default candidate but the choice is the maintainer's.

## 6. Change protocol

1. Additive changes (new nullable/defaulted columns, widened views) keep the floor
   stable → sink may land them without explorer changes (003 precedent).
2. Shape/semantic changes (001 FTS shape, 005 drops, 006 modes) require the sink
   migration **and** the explorer's contract-facing updates (capability handling,
   fixtures, tests) to be coordinated — same PR in a monorepo, or two same-day PRs
   plus a red drift job until the second lands under any polyrepo layout.
3. The drift-detect CI job (audit §10) is the enforcement: sink DDL changes
   without explorer sign-off must turn it red within a day.

## 7. Verification requirements

Adoption of this contract implies the explorer CI suite gains: the fixture
compatibility matrix (mode `off`/`body`/`full`), the drift-detect job, and a
sabotage canary proving the floor check fails when a required column disappears.
Job specs live in `docs/COUPLING-AUDIT.md` §10.
