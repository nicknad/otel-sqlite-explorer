// Package db provides a read-only SQLite access layer.
// No write operations are exposed.
package db

import (
	"database/sql"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

// Client holds the read-only SQLite connection.
type Client struct {
	db     *sql.DB
	mu     sync.Mutex
	hasFTS bool // true if a logs_fts table is present
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
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}

	// Test the connection
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("db ping: %w", err)
	}

	// Limit to 1 connection to keep things simple
	db.SetMaxOpenConns(1)

	// Detect whether an FTS5 full-text index (logs_fts) is available.
	// When absent, MatchExpr nodes are rewritten to body substring matches
	// by the API layer so the app keeps working on plain databases.
	var name string
	_ = db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='logs_fts' LIMIT 1").Scan(&name)

	return &Client{db: db, hasFTS: name == "logs_fts"}, nil
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
