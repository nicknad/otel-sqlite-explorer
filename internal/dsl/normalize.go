package dsl

import (
	"strings"
	"time"
)

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
	if q.Since != "" {
		d, err := time.ParseDuration(q.Since)
		if err == nil {
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
					Op:    OpAnd,
					Left:  tsExpr,
					Right: q.Where,
				}
			}
		}
		q.Since = "" // consumed
	}

	// --- Default sort: timestamp descending ---
	if len(q.Sort) == 0 {
		q.Sort = []Sort{{Field: "timestamp", Desc: true}}
	}
}
