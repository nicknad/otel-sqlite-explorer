// Package db provides a read-only SQLite access layer.
// No write operations are exposed.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite" // SQLite driver registration
)

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
// WAL safety.
func Open(path string) (*Client, error) {
	// URI format for modernc.org/sqlite
	dsn := fmt.Sprintf("file:%s?mode=ro&cache=shared", path)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("db open: %w", err)
	}

	// Safety and read-only pragmas
	pragmas := []string{
		"PRAGMA query_only = ON",
		"PRAGMA busy_timeout = 5000",
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(context.Background(), p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}

	// Test the connection
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("db ping: %w", err)
	}

	// Limit to 1 connection to keep things simple
	db.SetMaxOpenConns(1)

	// Detect whether an FTS5 full-text index (logs_fts) is available.
	// When absent, MatchExpr nodes are rewritten to body substring matches
	// by the API layer so the app keeps working on plain databases.
	var name string
	_ = db.QueryRowContext(context.Background(), "SELECT name FROM sqlite_master WHERE type='table' AND name='logs_fts' LIMIT 1").Scan(&name)

	return &Client{db: db, hasFTS: name == "logs_fts", timeout: DefaultQueryTimeout}, nil
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
