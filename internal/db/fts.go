package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// ftsState classifies the full-text index of an opened database. Detected
// once in Open; immutable afterwards.
type ftsState int

const (
	// ftsMissing: no usable logs_fts - absent, or present but not an fts5
	// virtual table. A plain table named logs_fts must never take MATCH.
	ftsMissing ftsState = iota
	// ftsUnmaintained: an fts5 logs_fts exists, but no trigger on log_event
	// writes into it. The sink's search mode 'off' lands here (table
	// present, empty, triggerless), as do one-shot backfilled indexes:
	// MATCH is syntactically valid but semantically dead.
	ftsUnmaintained
	// ftsActive: an fts5 logs_fts kept in sync by at least one trigger on
	// log_event. MATCH is meaningful. A sink index rebuild in flight does
	// not change this: the triggers exist while it runs, and partial
	// results are contractually normal (docs/CONTRACT.md), not degraded.
	ftsActive
)

// searchCapability is the cached detection result carried by Client.
// state is the single source of truth; reason records the derivation for
// tests and diagnostics. It has no setters: written once in Open.
type searchCapability struct {
	state  ftsState
	reason string
	// mode mirrors search_index_state.mode on 006+ sink databases: "off",
	// "body" or "full". Advisory only — deliberately not consulted for
	// classification (the triggers are the mechanism) — it exists so the
	// UI can say what MATCH actually covers instead of overclaiming.
	// "" on foreign and pre-006 databases.
	mode string
}

// Active is the one-bit projection every consumer uses.
func (s searchCapability) Active() bool { return s.state == ftsActive }

// ftsProbe is raw sqlite_master evidence, separated from classification so
// the decision rule is unit-testable without a database.
type ftsProbe struct {
	// createSQL is the CREATE statement of the table named logs_fts, ""
	// when no such table exists.
	createSQL string
	// maintTriggers counts triggers on log_event whose SQL references
	// logs_fts - the sink's logs_fts_ai/logs_fts_ad pair, or any
	// third-party equivalent.
	maintTriggers int
}

// Probe SQL - SELECTs only, safe under the read-only DSN. GLOB, not LIKE:
// GLOB is case-sensitive and treats '_' literally; LIKE would read '_' as
// a wildcard and match e.g. "logsXfts".
const (
	ftsTableProbeSQL   = "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'logs_fts'"
	ftsTriggerProbeSQL = "SELECT count(*) FROM sqlite_master " +
		"WHERE type = 'trigger' AND tbl_name = 'log_event' AND sql GLOB '*logs_fts*'"
)

// probeFTS gathers the evidence with two read-only queries. It never touches
// the index itself, so it works on contentless tables and empty indexes
// alike.
func probeFTS(ctx context.Context, db *sql.DB) (ftsProbe, error) {
	var p ftsProbe
	err := db.QueryRowContext(ctx, ftsTableProbeSQL).Scan(&p.createSQL)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return p, err
	}
	if err := db.QueryRowContext(ctx, ftsTriggerProbeSQL).Scan(&p.maintTriggers); err != nil {
		return p, err
	}
	return p, nil
}

// probeSearchMode mirrors search_index_state.mode when the sink's state row
// exists (006+). Advisory only: classification never consults it (the
// triggers are what keep MATCH true); the UI does, so its copy can say what
// the index actually covers. Foreign and pre-006 databases yield "".
func probeSearchMode(ctx context.Context, db *sql.DB) string {
	const q = "SELECT mode FROM search_index_state WHERE id = 1"
	var mode string
	if err := db.QueryRowContext(ctx, q).Scan(&mode); err != nil {
		return "" // absent table or row, or unreadable: unknown mode
	}
	return mode
}

// detectSearchCapability is the boundary wrapper called once from Open. A
// probe failure degrades to ftsMissing (substring fallback) rather than
// failing Open: capability detection is advisory, and the explorer must
// open any conforming SQLite database.
func detectSearchCapability(ctx context.Context, db *sql.DB) searchCapability {
	p, err := probeFTS(ctx, db)
	if err != nil {
		return searchCapability{state: ftsMissing, reason: "probe failed: " + err.Error()}
	}
	capability := classifyFTS(p)
	capability.mode = probeSearchMode(ctx, db)
	return capability
}

// classifyFTS is the pure decision rule:
//
//	active <=> logs_fts is an fts5 virtual table
//	           AND at least one trigger on log_event maintains it.
//
// Deliberately not consulted: search_index_state (the triggers are the
// mechanism that keeps MATCH true; the state row is the producer's mirror
// of it, and it does not exist on non-sink databases), the fts5 column set
// (the explorer emits table-level logs_fts MATCH and assumes no columns),
// and index row counts (a rebuilding index legitimately starts empty).
func classifyFTS(p ftsProbe) searchCapability {
	if p.createSQL == "" {
		return searchCapability{state: ftsMissing, reason: "no logs_fts table"}
	}
	if !isFts5VirtualTable(p.createSQL) {
		return searchCapability{state: ftsMissing,
			reason: "logs_fts is not an fts5 virtual table; refusing to MATCH a plain table"}
	}
	if p.maintTriggers == 0 {
		return searchCapability{state: ftsUnmaintained,
			reason: "logs_fts exists but no log_event trigger maintains it " +
				"(sink search mode 'off', or a one-shot backfill)"}
	}
	return searchCapability{state: ftsActive, reason: "fts5 logs_fts maintained by log_event triggers"}
}

// isFts5VirtualTable reports whether a CREATE statement declares an fts5
// virtual table. The check only needs to reject a plain table named
// logs_fts, and a miss degrades to the safe fallback, so a normalized
// substring match is enough - no SQL parser.
func isFts5VirtualTable(createSQL string) bool {
	normalized := strings.Join(strings.Fields(strings.ToLower(createSQL)), " ")
	return strings.Contains(normalized, "virtual table") &&
		strings.Contains(normalized, "using fts5")
}
