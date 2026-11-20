package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // SQLite driver registration
)

// requiredLogColumns lists the columns the read model (logs view/table)
// must expose; query compilation and row scanning assume this exact shape.
var requiredLogColumns = []string{
	"id", "timestamp_ns", "severity_text", "severity_number", "service_name",
	"trace_id", "span_id", "body", "attributes_json",
}

// DefaultQueryTimeout bounds every Execute call so a hung or oversized
// SQLite operation cannot block the HTTP handler forever. It exceeds the
// 5s busy_timeout so lock contention surfaces as "database is locked"
// rather than "context deadline exceeded".
const DefaultQueryTimeout = 10 * time.Second

// maxConcurrentReads bounds the read connection pool. The database file is
// written live by the sink, so the explorer keeps a small, deliberate reader
// footprint; concurrent queries share one page cache via cache=shared. If
// modernc surfaces table-lock errors under concurrency, drop cache=shared
// from the DSN (per-connection page caches are cheap at this pool size).
const maxConcurrentReads = 4

// Client holds the read-only SQLite access: a small pool of query
// connections plus one dedicated probe connection. The pool itself is the
// admission gate — database/sql queues QueryContext callers for a free
// connection while honoring their context, so a queued request counts down
// its own deadline (R4). The probe lane answers /healthz even while every
// query connection is busy (R5). The search capability is classified once at
// Open and immutable afterwards.
type Client struct {
	db      *sql.DB
	probe   *sql.DB
	search  searchCapability // classified once in Open; immutable afterwards
	timeout time.Duration
}

// Open opens a read-only connection to the SQLite database at path.
// The connection is configured for read-only mode, shared cache, and
// WAL safety; the pragmas are part of the DSN so they apply to every
// pooled connection, not just the one open executes them on.
func Open(path string) (*Client, error) {
	// Escape URI-reserved characters (?, #, %, spaces) in the path. Without
	// this, a '#' starts a URI fragment: the filename is truncated and the
	// mode=ro parameter is dropped, silently opening the wrong file
	// read-write.
	escapedPath := (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath()
	dsn := "file:" + escapedPath + "?mode=ro&cache=shared&_pragma=query_only(1)&_pragma=busy_timeout(5000)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("db open: %w", err)
	}

	// Test the connection; this also applies the DSN pragmas.
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("db ping: %w", err)
	}

	// A small pool of read connections: the pool is the admission gate
	// (database/sql honors each query's context while waiting for a free
	// connection), and concurrent readers share one page cache via
	// cache=shared.
	db.SetMaxOpenConns(maxConcurrentReads)

	// Reject databases without the expected read model up front: otherwise
	// the server starts, /healthz reports healthy, and every real query fails.
	if err := validateReadModel(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}

	// The probe lane: one dedicated read-only connection used only by
	// Ping, so liveness never queues behind queries (R5).
	probe, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("db open probe: %w", err)
	}
	probe.SetMaxOpenConns(1)
	if err := probe.PingContext(context.Background()); err != nil {
		_ = db.Close()
		_ = probe.Close()
		return nil, fmt.Errorf("db ping probe: %w", err)
	}

	// Classify the full-text search capability. A logs_fts table alone
	// proves nothing - the sink leaves one behind in every search mode,
	// including 'off', where nothing maintains it - so the capability reads
	// what actually keeps MATCH truthful: an fts5 virtual table maintained
	// by triggers on log_event. When it is unavailable, MatchExpr nodes are
	// rewritten to body substring matches by the query pipeline so the app
	// keeps working on plain databases.
	capability := detectSearchCapability(context.Background(), db)

	return &Client{db: db, probe: probe, search: capability, timeout: DefaultQueryTimeout}, nil
}

// validateReadModel checks that the database exposes a logs view/table with
// every required column, returning a descriptive error listing what is missing.
func validateReadModel(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info('logs')")
	if err != nil {
		return fmt.Errorf("inspect logs read model: %w", err)
	}
	defer func() { _ = rows.Close() }()

	present := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("inspect logs read model: %w", err)
		}
		present[strings.ToLower(name)] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect logs read model: %w", err)
	}
	if len(present) == 0 {
		return errors.New("database has no logs view or table; create the read model described in the README")
	}

	var missing []string
	for _, col := range requiredLogColumns {
		if !present[col] {
			missing = append(missing, col)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("logs read model is missing required column(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// Close shuts down both connection pools.
func (c *Client) Close() error {
	qerr := c.db.Close()
	perr := c.probe.Close()
	if qerr != nil {
		return qerr
	}
	return perr
}

// HasFTS reports whether a full-text MATCH on this database can be trusted:
// an fts5 logs_fts virtual table kept in sync by at least one trigger on
// log_event. It is false for the sink's search mode 'off' (the table exists
// but nothing maintains it), for indexes that are only backfilled, and for
// databases without usable FTS; callers fall back to substring matching.
func (c *Client) HasFTS() bool {
	return c.search.Active()
}

// SearchMode reports the sink's declared search mode ("off", "body", "full")
// when a search_index_state row exists; "" on foreign or pre-006 databases.
// Advisory, for honest UI copy; classification never consults it.
func (c *Client) SearchMode() string {
	return c.search.mode
}

// Ping verifies the database answers within the caller's budget. It backs the
// /healthz endpoint so orchestrators can probe liveness without running a
// real log query. The probe lane never queues behind queries: a
// busy-but-healthy instance answers healthy within its own deadline (R5), and
// if the sink's writer holds exclusive locks, busy_timeout makes the failure
// honest.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.probe.QueryRowContext(ctx, "SELECT 1").Err(); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	return nil
}

// ValidateMatch checks that query is valid FTS5 MATCH syntax without returning
// log rows. A valid query with no matches is not an error. Errors carry a
// message cleaned of driver noise ("SQL logic error: " prefix and the
// trailing "(<code>)" result code) so callers can surface them to clients
// without leaking internals; this is the driver's home, so the translation
// lives here.
func (c *Client) ValidateMatch(ctx context.Context, query string) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var one int
	err := c.db.QueryRowContext(ctx, "SELECT 1 FROM logs_fts WHERE logs_fts MATCH ? LIMIT 1", query).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // valid syntax, no matching rows
	}
	if err != nil {
		return errors.New(cleanDriverMessage(err.Error()))
	}
	return nil
}

// cleanDriverMessage strips driver noise ("SQL logic error: " prefix and the
// trailing "(<code>)" result code) from database errors surfaced to clients.
func cleanDriverMessage(msg string) string {
	msg = strings.TrimPrefix(msg, "SQL logic error: ")
	if i := strings.LastIndex(msg, " ("); i > 0 && strings.HasSuffix(msg, ")") {
		if _, convErr := strconv.Atoi(msg[i+2 : len(msg)-1]); convErr == nil {
			msg = msg[:i]
		}
	}
	return msg
}
