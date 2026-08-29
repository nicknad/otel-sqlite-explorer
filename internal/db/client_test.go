package db

import (
	"strings"
	"testing"
	"time"

	"log-explorer/internal/compiler"
)

// TestExecuteExpiredDeadlineFailsFast verifies that a query whose deadline
// has already passed is rejected without touching the database, so a
// zero/negative configured timeout cannot hang or panic the handler.
func TestExecuteExpiredDeadlineFailsFast(t *testing.T) {
	c := &Client{timeout: -time.Second}
	// The WithTimeout timer fires asynchronously; wait for it to elapse so
	// the fail-fast path is what rejects the query.
	time.Sleep(2 * time.Millisecond)

	_, err := c.Execute(&compiler.CompiledQuery{SQL: "SELECT 1"})
	if err == nil {
		t.Fatal("expected error for already-expired query deadline")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestDefaultQueryTimeoutIsPositive guards the production default so a
// regression cannot silently disable the query deadline.
func TestDefaultQueryTimeoutIsPositive(t *testing.T) {
	if DefaultQueryTimeout <= 0 {
		t.Errorf("DefaultQueryTimeout must be positive, got %v", DefaultQueryTimeout)
	}
}
