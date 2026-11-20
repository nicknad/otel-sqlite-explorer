// Package query is the explorer's query pipeline: the deep module that turns
// a log request — a form's filters or a DSL query — into a Page of log rows.
// It owns validation, search-shape selection (index search vs substring
// fallback), normalization, match-syntax checking, compilation, execution,
// and pagination; nothing else in the program sequences those steps.
package query

import (
	"context"
	"fmt"

	"log-explorer/internal/compiler"
	"log-explorer/internal/db"
	"log-explorer/internal/dsl"
)

// Executor is the seam the pipeline runs against. db.Client is the production
// adapter; tests supply an in-memory fake.
type Executor interface {
	// ExecuteContext runs one compiled query and returns log rows.
	ExecuteContext(ctx context.Context, cq *compiler.CompiledQuery) ([]db.LogRow, error)
	// HasFTS reports whether a full-text index is available. The fact's
	// source may change (search-capability detection); the decision to fall
	// back lives in this package either way.
	HasFTS() bool
	// ValidateMatch checks FTS5 MATCH syntax without returning rows.
	ValidateMatch(ctx context.Context, q string) error
}

// Pipeline composes validate → fallback → normalize → match-check → compile →
// execute, including the limit+1 pagination probe that stays internal.
type Pipeline struct{ exec Executor }

// New returns a pipeline running against exec.
func New(exec Executor) *Pipeline { return &Pipeline{exec: exec} }

// Page is the pipeline's single result: the rows of the requested window, the
// facts pagination needs, and the canonical echo of the filters applied.
type Page struct {
	Rows       []db.LogRow
	HasPrev    bool
	HasNext    bool
	PrevOffset int
	NextOffset int
	// Echo carries the canonicalized filter values the form re-renders.
	Echo Echo
}

// Echo is the canonicalized form echo: severity upper-cased, the effective
// limit/offset, and the raw since value (Normalize consumes Since, so the
// raw form value is preserved here for re-rendering).
type Echo struct {
	Severity string
	Limit    int
	Offset   int
	Since    string
}

// PageFromForm validates raw form filter values, assembles the DSL query they
// describe, and returns the page they select.
func (p *Pipeline) PageFromForm(ctx context.Context, f FormFilters) (Page, error) {
	q, echo, err := formQuery(f)
	if err != nil {
		return Page{}, err
	}
	return p.page(ctx, q, echo)
}

// PageFromDSL validates an already-decoded DSL query and returns the page it
// selects. The JSON API surface is unchanged: the handler encodes Rows and
// ignores the pagination facts.
func (p *Pipeline) PageFromDSL(ctx context.Context, q dsl.Query) (Page, error) {
	echo := Echo{Limit: q.Limit, Offset: q.Offset, Since: q.Since}
	return p.page(ctx, &q, echo)
}

// page runs the shared core. The order is load-bearing and lives only here:
// validate before the fallback rewrite (a match query must be valid before it
// is rewritten), normalize after the rewrite (default ordering must not
// override the fallback), match syntax checked after the shape is fixed, and
// the limit+1 probe executed directly so Normalize cannot clamp the headroom.
func (p *Pipeline) page(ctx context.Context, q *dsl.Query, echo Echo) (Page, error) {
	if err := dsl.Validate(q); err != nil {
		return Page{}, validationf("dsl", "%s", err.Error())
	}
	if !p.exec.HasFTS() && q.Where != nil {
		q.Where = dsl.RewriteMatchToContains(q.Where)
	}
	dsl.Normalize(q)
	echo.Limit, echo.Offset = q.Limit, q.Offset

	// The probe queries one extra row beyond the requested window. It is
	// compiled directly (never re-normalized) so requesting exactly
	// MaxLimit cannot clamp the headroom away — the next page stays
	// detectable precisely when the requested limit is MaxLimit.
	probe := *q
	probe.Limit = min(q.Limit+1, dsl.MaxLimit+1)
	if err := p.checkMatches(ctx, &probe); err != nil {
		return Page{}, err
	}
	cq, err := compiler.Compile(&probe)
	if err != nil {
		return Page{}, validationf("compile", "%s", err.Error())
	}
	rows, err := p.exec.ExecuteContext(ctx, cq)
	if err != nil {
		return Page{}, fmt.Errorf("execute: %w", err)
	}

	page := Page{Rows: rows, Echo: echo}
	page.HasNext = len(rows) > q.Limit
	if page.HasNext {
		page.Rows = rows[:q.Limit]
	}
	page.HasPrev = q.Offset > 0
	page.PrevOffset = max(q.Offset-q.Limit, 0)
	page.NextOffset = q.Offset + q.Limit
	return page, nil
}

// checkMatches rejects invalid FTS5 MATCH syntax up front when the search
// shape uses the index, so bad user syntax is the caller's fault (400), not a
// mid-query failure (500).
func (p *Pipeline) checkMatches(ctx context.Context, q *dsl.Query) error {
	if !p.exec.HasFTS() {
		return nil
	}
	for _, m := range dsl.MatchQueries(q.Where) {
		if err := p.exec.ValidateMatch(ctx, m); err != nil {
			return validationf("match", "invalid full-text search %q: %s", m, err.Error())
		}
	}
	return nil
}
