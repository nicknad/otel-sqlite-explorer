// Package migrate provides one-off, idempotent maintenance operations against
// the log database. It opens the database with write access and is therefore
// kept strictly separate from the read-only query sidecar. The sidecar itself
// never imports this package.
package migrate

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // SQLite driver registration
)

// SchemaSQL creates the FTS5 full-text index over the log content.
//
// A contentless FTS5 table (content=”) is used so the index can be built
// from a VIEW (the project's `logs` view joining log_event + log_resource)
// rather than requiring a real base table with a rowid. The index stores only
// tokens; rowids are supplied explicitly as the logs.id value during rebuild.
//
// Per the project constraints, NO triggers are created. The index is kept in
// sync by an explicit Rebuild call (run by this maintenance tool) rather than
// by SQLite triggers or application ingestion logic.
const SchemaSQL = `CREATE VIRTUAL TABLE IF NOT EXISTS logs_fts USING fts5(
	body,
	service_name,
	content=''
);`

// RebuildSQL is the logical population step: insert every log row into the
// contentless FTS5 index with its id as the rowid. The preceding
// DropSQL/CreateSQL clear the index atomically (contentless FTS5 does not
// support DELETE).
const RebuildSQL = `INSERT INTO logs_fts(rowid, body, service_name)
SELECT id, body, service_name FROM logs;`

// DropSQL drops the FTS5 index so it can be recreated cleanly. Used by
// RebuildFTS to make the rebuild idempotent for contentless tables.
const DropSQL = `DROP TABLE IF EXISTS logs_fts;`

// FTSStats describes the state of the FTS index relative to the logs table.
//
// For an external-content FTS5 table, COUNT(*) on logs_fts always mirrors the
// content table, so row counts alone cannot detect drift from ungated inserts.
// Use ProbeToken to verify whether a specific token is indexed.
type FTSStats struct {
	LogsCount int64 // rows in the logs table
	FTSCount  int64 // COUNT(*) on logs_fts (mirrors logs for external content)
	HasFTS    bool  // logs_fts table exists
}

// RebuildFTS (re)creates the FTS5 index and fully repopulates it from the
// logs view. It is idempotent and safe to rerun: the index is dropped and
// recreated within a single transaction so queries never see a partial index.
// Contentless FTS5 does not support DELETE, so drop+create is the canonical
// clear path.
//
// The connection must be writable (this package is invoked by the maintenance
// tool, never by the read-only sidecar).
func RebuildFTS(db *sql.DB) (FTSStats, error) {
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return FTSStats{}, fmt.Errorf("begin rebuild tx: %w", err)
	}
	if _, err := tx.ExecContext(context.Background(), DropSQL); err != nil {
		_ = tx.Rollback()
		return FTSStats{}, fmt.Errorf("drop fts: %w", err)
	}
	if _, err := tx.ExecContext(context.Background(), SchemaSQL); err != nil {
		_ = tx.Rollback()
		return FTSStats{}, fmt.Errorf("create fts: %w", err)
	}
	if _, err := tx.ExecContext(context.Background(), RebuildSQL); err != nil {
		_ = tx.Rollback()
		return FTSStats{}, fmt.Errorf("populate fts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return FTSStats{}, fmt.Errorf("commit rebuild: %w", err)
	}
	return Stats(db)
}

// Stats reports row counts for logs and logs_fts plus whether the index exists.
func Stats(db *sql.DB) (FTSStats, error) {
	var s FTSStats
	var name string
	err := db.QueryRowContext(
		context.Background(),
		"SELECT name FROM sqlite_master WHERE type='table' AND name='logs_fts' LIMIT 1",
	).Scan(&name)
	if err != nil && err != sql.ErrNoRows {
		return s, fmt.Errorf("check fts existence: %w", err)
	}
	s.HasFTS = name == "logs_fts"

	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM logs").Scan(&s.LogsCount); err != nil {
		return s, fmt.Errorf("count logs: %w", err)
	}
	if s.HasFTS {
		if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM logs_fts").Scan(&s.FTSCount); err != nil {
			return s, fmt.Errorf("count logs_fts: %w", err)
		}
	}
	return s, nil
}

// ProbeToken returns the number of rowids the FTS index resolves for a given
// FTS5 query string. It is the reliable way to detect drift with external-
// content tables: an ungated insert's tokens will not resolve until RebuildFTS
// is rerun. The query uses FTS5 syntax (e.g. "timeout", "error AND gateway").
func ProbeToken(db *sql.DB, ftsQuery string) (int64, error) {
	var n int64
	err := db.QueryRowContext(
		context.Background(),
		"SELECT COUNT(*) FROM (SELECT rowid FROM logs_fts WHERE logs_fts MATCH ?)",
		ftsQuery,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("probe token %q: %w", ftsQuery, err)
	}
	return n, nil
}
