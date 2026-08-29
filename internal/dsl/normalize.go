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
