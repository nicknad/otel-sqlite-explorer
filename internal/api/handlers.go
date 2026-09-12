// Package api implements the HTTP handlers for the log-explorer service.
// Handlers accept user input, construct DSL Query objects, and route them
// through the compile + execute pipeline.  No raw SQL touches this layer.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"log-explorer/internal/compiler"
	"log-explorer/internal/db"
	"log-explorer/internal/dsl"
	"log-explorer/internal/severity"
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
		"in": func(needle string, haystack ...string) bool {
			return slices.Contains(haystack, needle)
		},
		"hasAttributes": func(raw string) bool {
			var attrs map[string]json.RawMessage
			return json.Unmarshal([]byte(raw), &attrs) == nil && len(attrs) > 0
		},
		"prettyJSON": func(raw string) string {
			var formatted bytes.Buffer
			if err := json.Indent(&formatted, []byte(raw), "", "  "); err != nil {
				return raw
			}
			return formatted.String()
		},
	}

	tmpl, err := template.New("").Funcs(funcs).ParseFS(ui.Templates, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{db: database, tmpls: tmpl}, nil
}

// maxQueryBodyBytes caps JSON query request bodies so a single huge payload
// cannot exhaust server memory.
const maxQueryBodyBytes = 1 << 20 // 1 MiB

// ============================================================================
// Routes
// ============================================================================

// RegisterRoutes attaches handlers to the given mux.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	// The logs page doubles as the site index.
	mux.HandleFunc("GET /{$}", s.handleLogs)
	mux.HandleFunc("GET /logs", s.handleLogs)
	mux.HandleFunc("GET /logs/{id}", s.handleDetail)
	mux.HandleFunc("POST /logs/query", s.handleQuery)
	// Liveness probe for orchestrators; pings the database without running
	// a real log query.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	// Global stylesheet shared by every page, cacheable for an hour.
	static := http.FileServer(http.FS(ui.Static))
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		static.ServeHTTP(w, r)
	})))
	// Everything else: styled 404 page.
	mux.HandleFunc("GET /{path...}", func(w http.ResponseWriter, _ *http.Request) {
		s.renderError(w, http.StatusNotFound, "That page does not exist.")
	})
}

// Handler wraps mux with baseline security headers. The server entrypoint
// uses it; tests may serve the mux directly.
func (s *Server) Handler(mux http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
}

// ============================================================================
// Page data
// ============================================================================

// pageData carries everything the logs.html template needs to render the
// filter form (with preserved values) plus result rows and pagination state.
type pageData struct {
	Logs     []db.LogRow
	HasFTS   bool // full-text search available (logs_fts index present)
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
	// PrevOffset / NextOffset are the pager submit values; PrevOffset is
	// clamped to 0 so a partially-filled first page cannot page backwards.
	PrevOffset int
	NextOffset int
	HasPrev    bool
	HasNext    bool
}

// ============================================================================
// GET /logs  —  render the main UI page with default query results
// ============================================================================

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	pd, err := pageDataFromForm(r)
	if err != nil {
		s.renderError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.runQuery(r.Context(), &pd); err != nil {
		// User-supplied filter problems (bad severity, date, limit, …) are
		// the client's fault; anything else is a server-side failure.
		// The message is template-escaped on render, so echoing the input
		// back is XSS-safe.
		if isValidationError(err) {
			s.renderError(w, http.StatusBadRequest, err.Error())
			return
		}
		log.Printf("logs page query error: %v", err)
		s.renderError(w, http.StatusInternalServerError, "Internal error")
		return
	}
	s.renderLogsPage(w, &pd)
}

// isValidationError reports whether err stems from user input rather than a
// server-side failure. runQuery prefixes all input problems with
// "validation: ".
func isValidationError(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "validation: ")
}

// ============================================================================
// POST /logs/query  —  JSON query API (HTML rows when Accept omits JSON)
// ============================================================================

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		http.Error(w, "expected Content-Type: application/json", http.StatusUnsupportedMediaType)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxQueryBodyBytes)
	var q dsl.Query
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&q); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	// Exactly one JSON value: trailing data (e.g. `{...} extra`) must fail
	// loudly rather than being silently ignored.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON: unexpected data after query object", http.StatusBadRequest)
		return
	}
	if err := dsl.Validate(&q); err != nil {
		http.Error(w, fmt.Sprintf("validation: %v", err), http.StatusBadRequest)
		return
	}
	rows, err := s.executeQuery(r.Context(), &q)
	if err != nil {
		if isValidationError(err) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log.Printf("JSON query error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
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
}

// renderLogsPage writes the full logs.html page.
func (s *Server) renderLogsPage(w http.ResponseWriter, pd *pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpls.ExecuteTemplate(w, "logs.html", pd); err != nil {
		log.Printf("template error: %v", err)
	}
}

// renderError writes a styled error page with the given status and message.
// Used for browser-facing GET failures (unknown routes, bad/unknown log ids,
// internal query errors) so users get a consistent, navigable page.
func (s *Server) renderError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpls.ExecuteTemplate(w, "error.html", map[string]any{
		"Status":  http.StatusText(status),
		"Message": message,
	}); err != nil {
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
		s.renderError(w, http.StatusBadRequest, "The log id must be a positive integer.")
		return
	}

	cq := compiler.CompileGetByID(id)
	rows, err := s.db.ExecuteContext(r.Context(), cq)
	if err != nil {
		log.Printf("detail query error: %v", err)
		s.renderError(w, http.StatusInternalServerError, "Internal error")
		return
	}
	if len(rows) == 0 {
		s.renderError(w, http.StatusNotFound, "No log entry found with that id.")
		return
	}

	data := map[string]any{
		"Log": rows[0],
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
func (s *Server) runQuery(ctx context.Context, pd *pageData) error {
	pd.HasFTS = s.db.HasFTS()
	var exprs []dsl.Expr
	if pd.Service != "" {
		exprs = append(exprs, dsl.BinaryExpr{Op: dsl.OpContains, Field: "service_name", Value: dsl.Value{Type: dsl.ValueString, String: pd.Service}})
	}
	if pd.Severity != "" {
		n := severity.Number(pd.Severity)
		if n == 0 {
			return fmt.Errorf("validation: unknown severity %q (expected DEBUG, INFO, WARN, ERROR, or FATAL)", pd.Severity)
		}
		// Canonicalize so the form dropdown reflects the active filter even
		// when the user typed a lowercase severity in the URL.
		pd.Severity = strings.ToUpper(pd.Severity)
		exprs = append(exprs, dsl.BinaryExpr{
			Op:    dsl.OpGte,
			Field: "severity_number",
			Value: dsl.Value{Type: dsl.ValueInt, Int: n},
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
	// A date-only From starts at the beginning of that day; a date-only To
	// covers through the end of that day. Unparseable input is a client
	// error, not a silent no-op.
	if pd.DateFrom != "" {
		ts, err := parseDateTime(pd.DateFrom, false)
		if err != nil {
			return fmt.Errorf("validation: invalid date_from: %w", err)
		}
		exprs = append(exprs, dsl.BinaryExpr{Op: dsl.OpGte, Field: "timestamp", Value: dsl.Value{Type: dsl.ValueInt, Int: ts}})
	}
	if pd.DateTo != "" {
		ts, err := parseDateTime(pd.DateTo, true)
		if err != nil {
			return fmt.Errorf("validation: invalid date_to: %w", err)
		}
		exprs = append(exprs, dsl.BinaryExpr{Op: dsl.OpLte, Field: "timestamp", Value: dsl.Value{Type: dsl.ValueInt, Int: ts}})
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

	// Normalize the base query so the probe below uses effective defaults, and
	// mirror those values back onto the page data for form re-rendering.
	// q.Since is consumed by Normalize, so preserve the raw form value first.
	since := pd.Since
	s.rewriteMatchToContains(&q)
	dsl.Normalize(&q)
	pd.Limit = q.Limit
	pd.Offset = q.Offset
	pd.Since = since

	// Query one extra row to detect a next page. The probe is compiled and
	// executed directly (not through executeQuery) because re-normalizing it
	// would clamp limit+1 back to MaxLimit, hiding the next page exactly when
	// the requested limit is MaxLimit.
	probe := q
	probe.Limit = min(q.Limit+1, dsl.MaxLimit+1)
	rows, err := s.compileExecute(ctx, &probe)
	if err != nil {
		return err
	}

	pd.HasNext = len(rows) > q.Limit
	if pd.HasNext {
		rows = rows[:q.Limit]
	}
	pd.HasPrev = pd.Offset > 0
	pd.PrevOffset = max(pd.Offset-pd.Limit, 0)
	pd.NextOffset = pd.Offset + pd.Limit
	pd.Logs = rows
	return nil
}

// executeQuery runs a validated DSL query through the full pipeline: FTS
// fallback (when the backing DB has no logs_fts index), normalization,
// compilation, and execution. Execution failures are logged with their SQL
// and arguments server-side; the returned error carries only the database
// message so handlers can safely surface it without leaking internals.
func (s *Server) executeQuery(ctx context.Context, q *dsl.Query) ([]db.LogRow, error) {
	s.rewriteMatchToContains(q)
	dsl.Normalize(q)
	return s.compileExecute(ctx, q)
}

// compileExecute validates FTS syntax, compiles, and executes an
// already-normalized query. Callers that need the limit+1 headroom (the
// pagination probe) must use this directly so Normalize cannot clamp it.
func (s *Server) compileExecute(ctx context.Context, q *dsl.Query) ([]db.LogRow, error) {
	if err := s.validateMatches(ctx, q); err != nil {
		return nil, err
	}
	cq, err := compiler.Compile(q)
	if err != nil {
		return nil, fmt.Errorf("validation: %w", err)
	}
	rows, err := s.db.ExecuteContext(ctx, cq)
	if err != nil {
		log.Printf("query execute error: %v | SQL: %s | args: %v", err, cq.SQL, cq.Args)
		return nil, fmt.Errorf("execute: %w", err)
	}
	return rows, nil
}

// validateMatches rejects invalid FTS5 syntax up front: the search box passes
// user input straight to MATCH, and a query like `"unterminated` would
// otherwise fail inside the main query and surface as a 500.
func (s *Server) validateMatches(ctx context.Context, q *dsl.Query) error {
	if !s.db.HasFTS() {
		return nil
	}
	for _, m := range dsl.MatchQueries(q.Where) {
		if err := s.db.ValidateMatch(ctx, m); err != nil {
			return fmt.Errorf("validation: invalid full-text search %q: %s", m, cleanDBMessage(err))
		}
	}
	return nil
}

// cleanDBMessage strips driver noise ("SQL logic error: " prefix and the
// trailing "(<code>)" result code) from database errors surfaced to clients.
func cleanDBMessage(err error) string {
	msg := strings.TrimPrefix(err.Error(), "SQL logic error: ")
	if i := strings.LastIndex(msg, " ("); i > 0 && strings.HasSuffix(msg, ")") {
		if _, convErr := strconv.Atoi(msg[i+2 : len(msg)-1]); convErr == nil {
			msg = msg[:i]
		}
	}
	return msg
}

// ============================================================================
// GET /healthz  —  liveness probe (no log query)
// ============================================================================

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	if err := s.db.Ping(); err != nil {
		log.Printf("health check failed: %v", err)
		http.Error(w, "unhealthy", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ============================================================================
// Form → pageData conversion
// ============================================================================

// pageDataFromForm builds pageData from an HTTP GET query string, preserving
// all filter values so they can be re-rendered in the form. Unparseable or
// out-of-range limit/offset values are rejected instead of being silently
// ignored (which would disagree with the documented 400-on-invalid behavior).
func pageDataFromForm(r *http.Request) (pageData, error) {
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
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > dsl.MaxLimit {
			return pageData{}, fmt.Errorf("limit must be an integer between 1 and %d, or 0 for the default", dsl.MaxLimit)
		}
		if n > 0 {
			pd.Limit = n
		}
	}
	if v := r.FormValue("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return pageData{}, errors.New("offset must be a non-negative integer")
		}
		pd.Offset = n
	}
	return pd, nil
}

// parseDateTime parses a date or datetime string from the HTML form and returns
// the corresponding epoch nanoseconds (UTC). Supported formats:
//
//   - "2006-01-02T15:04"   (datetime-local, interpreted in the server's local
//     time zone, since browsers submit it without an offset)
//   - "2006-01-02T15:04:05" (same, with seconds)
//   - "2006-01-02"         (date only: start of day, or end of day when
//     endOfDay is true for an inclusive upper bound)
//   - RFC3339 / RFC3339Nano (e.g. "2024-01-15T14:30:00Z", offset honored)
func parseDateTime(s string, endOfDay bool) (int64, error) {
	// datetime-local carries no offset; interpret it in local time.
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return toUnixNano(t)
		}
	}
	// RFC3339 timestamps carry their own offset.
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return toUnixNano(t)
		}
	}
	// Date only — start or end of that day (UTC).
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return 0, fmt.Errorf("cannot parse date: %q", s)
	}
	if endOfDay {
		// End of day: 23:59:59.999999999 UTC
		end := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 999999999, time.UTC)
		return toUnixNano(end)
	}
	return toUnixNano(time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC))
}

// unixNanoMin/unixNanoMax bound the representable int64 nanosecond range
// (1677-09-21T00:12:43Z to 2262-04-11T23:47:16Z). time.Time.UnixNano is
// undefined outside it, so out-of-range form values must be rejected.
var (
	unixNanoMin = time.Unix(0, math.MinInt64).UTC()
	unixNanoMax = time.Unix(0, math.MaxInt64).UTC()
)

// toUnixNano converts t to UTC epoch nanoseconds, rejecting timestamps whose
// UnixNano would silently wrap.
func toUnixNano(t time.Time) (int64, error) {
	t = t.UTC()
	if t.Before(unixNanoMin) || t.After(unixNanoMax) {
		return 0, fmt.Errorf("%s is outside the supported range (1677-09-21 to 2262-04-11)", t.Format("2006-01-02"))
	}
	return t.UnixNano(), nil
}

// andExprs combines a slice of expressions into a single AND tree.
// For 0 expressions returns nil; for 1 returns it directly.
func andExprs(exprs []dsl.Expr) dsl.Expr {
	if len(exprs) == 0 {
		return nil
	}
	result := exprs[0]
	for _, e := range exprs[1:] {
		result = dsl.LogicalExpr{LogicalOp: dsl.OpAnd, Left: result, Right: e}
	}
	return result
}

// rewriteMatchToContains rewrites any MatchExpr nodes to body substring
// matches when the backing database has no logs_fts full-text index. Must run
// after Validate and before Normalize so default ordering is applied correctly
// for the fallback path.
func (s *Server) rewriteMatchToContains(q *dsl.Query) {
	if s.db.HasFTS() || q.Where == nil {
		return
	}
	q.Where = dsl.RewriteMatchToContains(q.Where)
}
