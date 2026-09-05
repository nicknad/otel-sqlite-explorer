package dsl

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseSince parses a `since` duration window. In addition to every unit
// accepted by time.ParseDuration (e.g. "24h", "90m"), it accepts whole-day
// ("7d") and whole-week ("1w") shorthands documented in the README.
// Non-positive durations are rejected.
func ParseSince(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return 0, fmt.Errorf("duration must be positive, got %q", s)
		}
		return d, nil
	}
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	unit := s[len(s)-1]
	if unit != 'd' && unit != 'w' {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	d := time.Duration(n) * 24 * time.Hour
	if unit == 'w' {
		d *= 7
	}
	return d, nil
}

// Normalize applies defaults and transforms to a Query before compilation.
// It modifies q in place.
func Normalize(q *Query) {
	// --- Select ---
	if strings.TrimSpace(q.Select) == "" {
		q.Select = "*"
	}

	// --- Limit defaults ---
	if q.Limit <= 0 {
		q.Limit = DefaultLimit
	}
	if q.Limit > MaxLimit {
		q.Limit = MaxLimit // safety clamp
	}
	if q.Offset < 0 {
		q.Offset = 0
	}

	// --- Since duration → timestamp expression ---
	// Validate guarantees q.Since parses; a parse failure here is ignored
	// defensively so Normalize never drops the rest of the query.
	if q.Since != "" {
		if d, err := ParseSince(q.Since); err == nil {
			threshold := time.Now().Add(-d).UnixNano()
			tsExpr := BinaryExpr{
				Op:    OpGte,
				Field: "timestamp",
				Value: Value{
					Type: ValueInt,
					Int:  threshold,
				},
			}
			if q.Where == nil {
				q.Where = tsExpr
			} else {
				q.Where = LogicalExpr{
					LogicalOp: OpAnd,
					Left:      tsExpr,
					Right:     q.Where,
				}
			}
		}
		q.Since = "" // consumed
	}

	// --- Default sort: timestamp descending ---
	// Skip when a full-text match is present so the compiler can apply
	// bm25 relevance ranking instead.
	if len(q.Sort) == 0 && !HasMatch(q.Where) {
		q.Sort = []Sort{{Field: "timestamp", Desc: true}}
	}
}

// RewriteMatchToContains replaces every MatchExpr in the tree with a
// body substring (contains) expression. It is used as a graceful fallback
// when the backing database has no logs_fts index, preserving the
// "inclusion" search behaviour over body text.
func RewriteMatchToContains(e Expr) Expr {
	switch x := e.(type) {
	case MatchExpr:
		return BinaryExpr{
			Op:    OpContains,
			Field: "body",
			Value: Value{Type: ValueString, String: x.Query},
		}
	case LogicalExpr:
		x.Left = RewriteMatchToContains(x.Left)
		x.Right = RewriteMatchToContains(x.Right)
		return x
	}
	return e
}
