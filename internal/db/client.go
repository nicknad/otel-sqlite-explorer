// Package db provides a read-only SQLite access layer.
// No write operations are exposed.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // SQLite driver registration
)

// requiredLogColumns lists the columns the read model (`logs` view/table)
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

// Client holds the read-only SQLite connection.
type Client struct {
	db      *sql.DB
	mu      sync.Mutex
	hasFTS  bool // true if a logs_fts table is present
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

	// Limit to 1 connection to keep things simple
	db.SetMaxOpenConns(1)

	// Reject databases without the expected read model up front: otherwise
	// the server starts, /healthz reports healthy, and every real query fails.
	if err := validateReadModel(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}

	// Detect whether an FTS5 full-text index (logs_fts) is available.
	// When absent, MatchExpr nodes are rewritten to body substring matches
	// by the API layer so the app keeps working on plain databases.
	var name string
	_ = db.QueryRowContext(context.Background(), "SELECT name FROM sqlite_master WHERE type='table' AND name='logs_fts' LIMIT 1").Scan(&name)

	return &Client{db: db, hasFTS: name == "logs_fts", timeout: DefaultQueryTimeout}, nil
}

// validateReadModel checks that the database exposes a `logs` view/table with
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
		return errors.New("database has no `logs` view or table; create the read model described in the README")
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

// DB returns the underlying *sql.DB for query execution.
// Callers must hold the mutex for thread safety.
func (c *Client) DB() *sql.DB {
	return c.db
}

// Close shuts down the database connection.
func (c *Client) Close() error {
	return c.db.Close()
}

// HasFTS reports whether the database exposes a logs_fts full-text index.
func (c *Client) HasFTS() bool {
	return c.hasFTS
}

// Ping verifies the database answers within a short deadline. It backs the
// /healthz endpoint so orchestrators can probe liveness without running a
// real log query.
func (c *Client) Ping() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.db.QueryRowContext(ctx, "SELECT 1").Err(); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	return nil
}

// ValidateMatch checks that query is valid FTS5 MATCH syntax without returning
// log rows. A valid query with no matches is not an error. This lets the API
// layer report bad user syntax as a 400 instead of a database failure.
func (c *Client) ValidateMatch(ctx context.Context, query string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var one int
	err := c.db.QueryRowContext(ctx, "SELECT 1 FROM logs_fts WHERE logs_fts MATCH ? LIMIT 1", query).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // valid syntax, no matching rows
	}
	return err
}
