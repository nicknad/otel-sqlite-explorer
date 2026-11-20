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
	"io/fs"
	"log"
	"mime"
	"net/http"
	"path"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"log-explorer/internal/compiler"
	"log-explorer/internal/db"
	"log-explorer/internal/dsl"
	"log-explorer/internal/query"
	"log-explorer/internal/ui"
)

// Server holds shared state for HTTP handlers: the read-only database (for
// the detail view's direct query and FTS availability) and the query pipeline
// every log request runs through.
type Server struct {
	db    *db.Client
	pipe  *query.Pipeline
	tmpls *template.Template
}

// NewServer creates a Server with a live DB connection and parsed templates.
func NewServer(database *db.Client) (*Server, error) {
	// Register custom template functions.
	funcs := template.FuncMap{
		"nstime": func(ns int64) string {
			return time.Unix(0, ns).UTC().Format("2006-01-02 15:04:05.000")
		},
		"nstimeFull": func(ns int64) string {
			return time.Unix(0, ns).UTC().Format(time.RFC3339Nano)
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
	return &Server{db: database, pipe: query.New(database), tmpls: tmpl}, nil
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
	// Global static assets, cacheable for an hour. Directory paths are
	// rejected so the embedded FS cannot be browsed.
	static := http.FileServer(http.FS(ui.Static))
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			http.NotFound(w, r)
			return
		}
		info, err := fs.Stat(ui.Static, name)
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		static.ServeHTTP(w, r)
	})))
	// Everything else: styled 404 page.
	mux.HandleFunc("GET /{path...}", func(w http.ResponseWriter, _ *http.Request) {
		s.renderError(w, http.StatusNotFound, "That page does not exist.")
	})
}

// responseTracker records whether anything was written so panic recovery
// does not emit a superfluous WriteHeader after a partial response.
type responseTracker struct {
	http.ResponseWriter
	wrote bool
}

func (t *responseTracker) WriteHeader(code int) {
	if !t.wrote {
		t.wrote = true
		t.ResponseWriter.WriteHeader(code)
	}
}

func (t *responseTracker) Write(b []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(b)
}

// Handler wraps mux with baseline security headers, a default no-store cache
// policy, and panic recovery. Static assets override the cache policy.
func (s *Server) Handler(mux http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "no-referrer")
		// All assets are self-hosted; inline styles appear in templates but no
		// inline scripts are used.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'self'")
		// Log payloads may be sensitive; do not let intermediaries persist them.
		w.Header().Set("Cache-Control", "no-store")

		tw := &responseTracker{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil && rec != http.ErrAbortHandler {
				// strconv.Quote neutralizes newlines and control characters,
				// so the tainted path cannot forge log entries.
				log.Printf("panic serving %s %s: %v\n%s", r.Method, strconv.Quote(r.URL.Path), rec, debug.Stack()) // #nosec G706 -- request path is quoted
				if !tw.wrote {
					http.Error(tw, "Internal server error", http.StatusInternalServerError)
					return
				}
			}
		}()
		mux.ServeHTTP(tw, r)
	})
}

// ============================================================================
// Page data
// ============================================================================

// pageData carries everything the logs.html template needs to render the
// filter form (with preserved values) plus result rows and pagination state.
type pageData struct {
	Logs       []db.LogRow
	HasFTS     bool   // full-text search available (a maintained logs_fts index)
	SearchMode string // sink-declared index mode: "off", "body", "full"; "" when unknown
	Service    string
	Severity   string
	TraceID    string
	SpanID     string
	Body       string
	Search     string // full-text (FTS5) query; maps to a MatchExpr
	Since      string
	DateFrom   string // RFC3339 / datetime-local start (inclusive)
	DateTo     string // RFC3339 / datetime-local end (inclusive)
	Limit      int
	Offset     int
	// PrevOffset / NextOffset are the pager submit values; PrevOffset is
	// clamped to 0 so a partially-filled first page cannot page backwards.
	PrevOffset int
	NextOffset int
	HasPrev    bool
	HasNext    bool
	// QuerySuffix is the canonical "?a=b&c=d" form of the active filters, used
	// to preserve state in row and back links. Empty when there are none.
	QuerySuffix string
}

// ============================================================================
// GET /logs  —  render the main UI page with default query results
// ============================================================================

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	filters := formFiltersFromRequest(r)
	page, err := s.pipe.PageFromForm(r.Context(), filters)
	if err != nil {
		// User-supplied filter problems are the client's fault; anything
		// else is a server-side failure. The message is template-escaped on
		// render, so echoing the input back is XSS-safe.
		var ve *query.ValidationError
		if errors.As(err, &ve) {
			// Byte-identity: every rule except the form's limit/offset parse
			// errors rendered with the "validation: " prefix; those two never
			// did. Both framings reproduce here from the typed error.
			msg := ve.Error()
			if ve.Rule == "limit" || ve.Rule == "offset" {
				msg = ve.Detail
			}
			s.renderError(w, http.StatusBadRequest, msg)
			return
		}
		log.Printf("logs page query error: %v", err)
		s.renderError(w, http.StatusInternalServerError, "Internal error")
		return
	}
	pd := pageDataFromFilters(r, filters, page)
	pd.HasFTS = s.db.HasFTS()
	pd.SearchMode = s.db.SearchMode()
	s.renderLogsPage(w, &pd)
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
	page, err := s.pipe.PageFromDSL(r.Context(), q)
	if err != nil {
		var ve *query.ValidationError
		if errors.As(err, &ve) {
			// Byte-identity: the legacy body carried the "validation: "
			// prefix for every rule on this path.
			http.Error(w, ve.Error(), http.StatusBadRequest)
			return
		}
		log.Printf("JSON query error: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	rows := page.Rows
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
		"Log":         rows[0],
		"QuerySuffix": querySuffix(r),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpls.ExecuteTemplate(w, "detail.html", data); err != nil {
		log.Printf("template error: %v", err)
	}
}

// ============================================================================
// GET /healthz  —  liveness probe (no log query)
// ============================================================================

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// The probe owns its budget: two seconds regardless of query load.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
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

// formFiltersFromRequest extracts the raw filter values of the logs form.
// Text filters are trimmed exactly as the legacy form parser did; Since,
// Limit and Offset keep their raw values (their rules live in the pipeline).
func formFiltersFromRequest(r *http.Request) query.FormFilters {
	_ = r.ParseForm()
	return query.FormFilters{
		Service:  strings.TrimSpace(r.FormValue("service_name")),
		Severity: strings.TrimSpace(r.FormValue("severity")),
		TraceID:  strings.TrimSpace(r.FormValue("trace_id")),
		SpanID:   strings.TrimSpace(r.FormValue("span_id")),
		Body:     strings.TrimSpace(r.FormValue("body")),
		Search:   strings.TrimSpace(r.FormValue("search")),
		Since:    r.FormValue("since"),
		DateFrom: strings.TrimSpace(r.FormValue("date_from")),
		DateTo:   strings.TrimSpace(r.FormValue("date_to")),
		Limit:    r.FormValue("limit"),
		Offset:   r.FormValue("offset"),
	}
}

// pageDataFromFilters builds the template view: filter inputs re-render their
// raw values, while severity, limit, offset and since come from the
// pipeline's canonical echo.
func pageDataFromFilters(r *http.Request, f query.FormFilters, page query.Page) pageData {
	return pageData{
		Logs:        page.Rows,
		Service:     f.Service,
		Severity:    page.Echo.Severity,
		TraceID:     f.TraceID,
		SpanID:      f.SpanID,
		Body:        f.Body,
		Search:      f.Search,
		Since:       page.Echo.Since,
		DateFrom:    f.DateFrom,
		DateTo:      f.DateTo,
		Limit:       page.Echo.Limit,
		Offset:      page.Echo.Offset,
		PrevOffset:  page.PrevOffset,
		NextOffset:  page.NextOffset,
		HasPrev:     page.HasPrev,
		HasNext:     page.HasNext,
		QuerySuffix: querySuffix(r),
	}
}

// querySuffix returns the active query string as a canonical "?a=b&c=d"
// suffix (sorted, properly encoded), or "" when there are no parameters. The
// whole suffix is interpolated into href attributes so html/template does not
// percent-encode the separators.
func querySuffix(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.Query().Encode()
}
