// Package api implements the HTTP handlers for the log-explorer service.
// Handlers accept user input, construct DSL Query objects, and route them
// through the compile + execute pipeline.  No raw SQL touches this layer.
package api

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"log-explorer/internal/compiler"
	"log-explorer/internal/db"
	"log-explorer/internal/dsl"
	"log-explorer/internal/ui"
)

// Server holds shared state for HTTP handlers.
type Server struct {
	db    *db.Client
	tmpls *template.Template
}

// NewServer creates a Server with a live DB connection and parsed templates.
func NewServer(database *db.Client) (*Server, error) {
	// Register custom template functions.
	funcs := template.FuncMap{
		"nstime": func(ns int64) string {
			return time.Unix(0, ns).UTC().Format("2006-01-02 15:04:05.000")
		},
		"lower": strings.ToLower,
		"add":   func(a, b int) int { return a + b },
		"sub":   func(a, b int) int { return a - b },
	}

	tmpl, err := template.New("").Funcs(funcs).ParseFS(ui.Templates, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{db: database, tmpls: tmpl}, nil
}

// ============================================================================
// Routes
// ============================================================================

// RegisterRoutes attaches handlers to the given mux.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /logs", s.handleLogs)
	mux.HandleFunc("GET /logs/{id}", s.handleDetail)
	mux.HandleFunc("POST /logs/query", s.handleQuery)
}

// ============================================================================
// Page data
// ============================================================================

// pageData carries everything the logs.html template needs to render the
// filter form (with preserved values) plus result rows and pagination state.
type pageData struct {
	Logs     []db.LogRow
	Service  string
	Severity string
	TraceID  string
	SpanID   string
	Body     string
	Search   string // full-text (FTS5) query; maps to a MatchExpr
	Since    string
	DateFrom string // RFC3339 / datetime-local start (inclusive)
	DateTo   string // RFC3339 / datetime-local end (inclusive)
	Limit    int
	Offset   int
	HasPrev  bool
	HasNext  bool
}

// ============================================================================
// GET /logs  —  render the main UI page with default query results
// ============================================================================

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	pd := pageDataFromForm(r)
	if err := s.runQuery(&pd); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	s.renderLogsPage(w, &pd)
}

// ============================================================================
// POST /logs/query  —  HTMX partial, JSON, or full page fallback
// ============================================================================

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")

	// --- JSON API path ---
	if strings.Contains(ct, "application/json") {
		var q dsl.Query
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
			return
		}
		if err := dsl.Validate(&q); err != nil {
			http.Error(w, fmt.Sprintf("validation: %v", err), http.StatusBadRequest)
			return
		}
		s.applyFTSFallback(&q)
		dsl.Normalize(&q)
		cq, err := compiler.Compile(&q)
		if err != nil {
			http.Error(w, fmt.Sprintf("compile: %v", err), http.StatusBadRequest)
			return
		}
		rows, err := s.db.Execute(cq)
		if err != nil {
			http.Error(w, fmt.Sprintf("execute: %v", err), http.StatusBadRequest)
			return
		}
		accept := r.Header.Get("Accept")
		if strings.Contains(accept, "application/json") {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			if err := json.NewEncoder(w).Encode(rows); err != nil {
				log.Printf("encode JSON: %v", err)
			}
			return
		}
		// JSON in, HTML rows out (partial).
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := s.tmpls.ExecuteTemplate(w, "rows", map[string]any{"Logs": rows}); err != nil {
			log.Printf("template: %v", err)
		}
		return
	}

	// --- Form path ---
	pd := pageDataFromForm(r)
	if err := s.runQuery(&pd); err != nil {
		log.Printf("query error: %v", err)
		http.Error(w, fmt.Sprintf("Query error: %v", err), http.StatusBadRequest)
		return
	}

	// Both HTMX and plain-POST get the full page. HTMX extracts #results
	// via hx-select and swaps it in; plain POST re-renders the whole page.
	s.renderLogsPage(w, &pd)
}

// renderLogsPage writes the full logs.html page.
func (s *Server) renderLogsPage(w http.ResponseWriter, pd *pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpls.ExecuteTemplate(w, "logs.html", pd); err != nil {
		log.Printf("template error: %v", err)
	}
}

// ============================================================================
// GET /logs/{id}  —  render the full payload of a single log entry
// ============================================================================

func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	cq := compiler.CompileGetByID(id)
	rows, err := s.db.Execute(cq)
	if err != nil {
		log.Printf("detail query error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if len(rows) == 0 {
		http.Error(w, "log not found", http.StatusNotFound)
		return
	}

	aq := compiler.CompileGetAttrs(id)
	attrs, err := s.db.GetAttrs(aq)
	if err != nil {
		log.Printf("attrs query error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	data := map[string]any{
		"Log":   rows[0],
		"Attrs": attrs,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpls.ExecuteTemplate(w, "detail.html", data); err != nil {
		log.Printf("template error: %v", err)
	}
}

// ============================================================================
// Query pipeline
// ============================================================================

// runQuery builds a DSL query from the page data, executes it, and fills in
// pd.Logs plus pagination flags (HasPrev / HasNext). It queries limit+1 rows
// to detect whether a next page exists.
func (s *Server) runQuery(pd *pageData) error {
	var exprs []dsl.Expr
	if pd.Service != "" {
		exprs = append(exprs, dsl.BinaryExpr{Op: dsl.OpContains, Field: "service_name", Value: dsl.Value{Type: dsl.ValueString, String: pd.Service}})
	}
	if pd.Severity != "" {
		threshold := severityNumberThreshold(pd.Severity)
		exprs = append(exprs, dsl.BinaryExpr{
			Op:    dsl.OpGte,
			Field: "severity_number",
			Value: dsl.Value{Type: dsl.ValueInt, Int: threshold},
		})
	}
	if pd.TraceID != "" {
		exprs = append(exprs, dsl.BinaryExpr{Op: dsl.OpEq, Field: "trace_id", Value: dsl.Value{Type: dsl.ValueString, String: pd.TraceID}})
	}
	if pd.SpanID != "" {
		exprs = append(exprs, dsl.BinaryExpr{Op: dsl.OpEq, Field: "span_id", Value: dsl.Value{Type: dsl.ValueString, String: pd.SpanID}})
	}
	if pd.Body != "" {
		exprs = append(exprs, dsl.BinaryExpr{Op: dsl.OpContains, Field: "body", Value: dsl.Value{Type: dsl.ValueString, String: pd.Body}})
	}
	// Date range filters — convert to timestamp nanoseconds for DSL.
	if pd.DateFrom != "" {
		ts, err := parseDateTime(pd.DateFrom)
		if err == nil {
			exprs = append(exprs, dsl.BinaryExpr{Op: dsl.OpGte, Field: "timestamp", Value: dsl.Value{Type: dsl.ValueInt, Int: ts}})
		}
	}
	if pd.DateTo != "" {
		ts, err := parseDateTime(pd.DateTo)
		if err == nil {
			// End of the given day (add 24h) when only a date was entered;
			// parseDateTime already handles this by appending 23:59:59.
			exprs = append(exprs, dsl.BinaryExpr{Op: dsl.OpLte, Field: "timestamp", Value: dsl.Value{Type: dsl.ValueInt, Int: ts}})
		}
	}
	if pd.Search != "" {
		exprs = append(exprs, dsl.MatchExpr{Query: pd.Search})
	}

	q := dsl.Query{
		Select: "*",
		Since:  pd.Since,
		Limit:  pd.Limit,
		Offset: pd.Offset,
	}
	if len(exprs) > 0 {
		q.Where = andExprs(exprs)
	}

	if err := dsl.Validate(&q); err != nil {
		return fmt.Errorf("validation: %w", err)
	}
	s.applyFTSFallback(&q)
	dsl.Normalize(&q)

	// Sync normalized defaults back to page data.
	pd.Limit = q.Limit
	pd.Offset = q.Offset
	pd.Since = q.Since

	// Query one extra row to detect a next page.
	probe := q
	probe.Limit = min(q.Limit+1, dsl.MaxLimit+1)
	cq, err := compiler.Compile(&probe)
	if err != nil {
		return fmt.Errorf("compile: %w", err)
	}
	rows, err := s.db.Execute(cq)
	if err != nil {
		return fmt.Errorf("execute: %w", err)
	}

	pd.HasNext = len(rows) > q.Limit
	if pd.HasNext {
		rows = rows[:q.Limit]
	}
	pd.HasPrev = pd.Offset > 0
	pd.Logs = rows
	return nil
}

// ============================================================================
// Form → pageData conversion
// ============================================================================

// pageDataFromForm builds pageData from an HTTP POST form, preserving all
// filter values so they can be re-rendered in the form.
func pageDataFromForm(r *http.Request) pageData {
	_ = r.ParseForm()
	pd := pageData{
		Service:  strings.TrimSpace(r.FormValue("service_name")),
		Severity: strings.TrimSpace(r.FormValue("severity")),
		TraceID:  strings.TrimSpace(r.FormValue("trace_id")),
		SpanID:   strings.TrimSpace(r.FormValue("span_id")),
		Body:     strings.TrimSpace(r.FormValue("body")),
		Search:   strings.TrimSpace(r.FormValue("search")),
		Since:    r.FormValue("since"),
		DateFrom: strings.TrimSpace(r.FormValue("date_from")),
		DateTo:   strings.TrimSpace(r.FormValue("date_to")),
		Limit:    100,
		Offset:   0,
	}
	if v := r.FormValue("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			pd.Limit = n
		}
	}
	if v := r.FormValue("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			pd.Offset = n
		}
	}
	if pd.Since == "" && !hasFormParams(r) {
		pd.Since = "24h" // default only on a bare GET /logs with no params
	}
	return pd
}

// hasFormParams reports whether the request carries any filter params,
// distinguishing a bare page load (apply 24h default) from an explicit
// "All time" selection (empty since, but other filters present).
func hasFormParams(r *http.Request) bool {
	return slices.ContainsFunc([]string{"service_name", "severity", "trace_id", "span_id", "body", "search", "since", "date_from", "date_to", "limit", "offset"}, r.Form.Has)
}

// parseDateTime parses a date or datetime string from the HTML form and returns
// the corresponding epoch nanoseconds (UTC). Supported formats:
//   - "2006-01-02T15:04"   (datetime-local, local time, stored as UTC)
//   - "2006-01-02"         (date only, treated as end-of-day 23:59:59)
//   - RFC3339 / RFC3339Nano (e.g. "2024-01-15T14:30:00Z")
func parseDateTime(s string) (int64, error) {
	// Try RFC3339 first (with or without TZ).
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04"} {
		t, err := time.Parse(layout, s)
		if err == nil {
			return t.UTC().UnixNano(), nil
		}
	}
	// Date only — treat as end of that day (23:59:59 UTC).
	t, err := time.Parse("2006-01-02", s)
	if err == nil {
		// End of day: 23:59:59.999999999 UTC
		endOfDay := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 999999999, time.UTC)
		return endOfDay.UnixNano(), nil
	}
	return 0, fmt.Errorf("cannot parse date: %q", s)
}

// severityNumberThreshold maps a user-visible severity label to the
// corresponding OTel severity_number threshold for "this level and above"
// filtering (OpGte). Thresholds match the iota values defined in
// otel-sqlite/internal/model/logrecord.go:
//
//	Debug=5, Info=9, Warn=13, Error=17, Fatal=21
func severityNumberThreshold(severity string) int64 {
	switch strings.ToUpper(strings.TrimSpace(severity)) {
	case "DEBUG":
		return 5
	case "INFO":
		return 9
	case "WARN":
		return 13
	case "ERROR":
		return 17
	default:
		return 0
	}
}

// andExprs combines a slice of expressions into a single AND tree.
// For 0 expressions returns nil; for 1 returns it directly.
func andExprs(exprs []dsl.Expr) dsl.Expr {
	if len(exprs) == 0 {
		return nil
	}
	result := exprs[0]
	for _, e := range exprs[1:] {
		result = dsl.LogicalExpr{Op: dsl.OpAnd, Left: result, Right: e}
	}
	return result
}

// applyFTSFallback rewrites any MatchExpr nodes to body substring matches when
// the backing database has no logs_fts full-text index. Must run after Validate
// and before Normalize so default ordering is applied correctly for the
// fallback path.
func (s *Server) applyFTSFallback(q *dsl.Query) {
	if s.db.HasFTS() || q.Where == nil {
		return
	}
	q.Where = dsl.RewriteMatchToContains(q.Where)
}
