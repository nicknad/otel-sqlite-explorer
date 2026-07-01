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
	Since    string
	Limit    int
	Offset   int
	HasPrev  bool
	HasNext  bool
}

// ============================================================================
// GET /logs  —  render the main UI page with default query results
// ============================================================================

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	// Read filters from URL query params so reloads / shared URLs preserve state.
	// When no params are present, pageDataFromForm applies the defaults.
	pd := pageDataFromForm(r)
	if err := s.runQuery(&pd); err != nil {
		log.Printf("default query error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	s.renderLogsPage(w, pd)
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
			json.NewEncoder(w).Encode(rows)
			return
		}
		// JSON in, HTML rows out (partial).
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		s.tmpls.ExecuteTemplate(w, "rows", map[string]any{"Logs": rows})
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
	s.renderLogsPage(w, pd)
}

// renderLogsPage writes the full logs.html page.
func (s *Server) renderLogsPage(w http.ResponseWriter, pd pageData) {
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
		exprs = append(exprs, dsl.BinaryExpr{Op: dsl.OpEq, Field: "severity", Value: dsl.Value{Type: dsl.ValueString, String: strings.ToUpper(pd.Severity)}})
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
	dsl.Normalize(&q)

	// Sync normalized defaults back to page data.
	pd.Limit = q.Limit
	pd.Offset = q.Offset
	pd.Since = q.Since

	// Query one extra row to detect a next page.
	probe := q
	probe.Limit = q.Limit + 1
	if probe.Limit > dsl.MaxLimit+1 {
		probe.Limit = dsl.MaxLimit + 1
	}
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
		Since:    r.FormValue("since"),
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
	if pd.Since == "" {
		pd.Since = "24h"
	}
	return pd
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
