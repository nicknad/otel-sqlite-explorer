package tests

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"log-explorer/internal/api"
	"log-explorer/internal/compiler"
	"log-explorer/internal/db"
	"log-explorer/internal/dsl"
)

// ==========================================================================
// E2E test harness
// ==========================================================================

type e2eHarness struct {
	server *httptest.Server
	client *db.Client
	dbPath string
}

func newE2EHarness(t *testing.T, withFTS bool) *e2eHarness {
	t.Helper()

	path := setupTestDB(t)
	if withFTS {
		path = setupTestDBWithFTS(t)
	}

	client, err := db.Open(path)
	if err != nil {
		_ = os.Remove(path)
		t.Fatalf("open db: %v", err)
	}

	srv, err := api.NewServer(client)
	if err != nil {
		_ = client.Close()
		_ = os.Remove(path)
		t.Fatalf("create server: %v", err)
	}

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	// Serve through the production middleware stack (security headers), not
	// the bare mux, so tests observe what clients observe.
	ts := httptest.NewServer(srv.Handler(mux))
	t.Cleanup(func() {
		ts.Close()
		_ = client.Close()
		_ = os.Remove(path)
	})

	return &e2eHarness{server: ts, client: client, dbPath: path}
}

// get performs a GET request. The path may include a query string.
func (h *e2eHarness) get(t *testing.T, pathAndQuery string) *http.Response {
	t.Helper()
	resp, err := h.server.Client().Get(h.server.URL + pathAndQuery)
	if err != nil {
		t.Fatalf("GET %s: %v", pathAndQuery, err)
	}
	return resp
}

// getStatus performs a GET request and returns the status code without
// touching testing.T, so it is safe to call from worker goroutines (FailNow
// must only run on the test goroutine).
func (h *e2eHarness) getStatus(pathAndQuery string) (int, error) {
	resp, err := h.server.Client().Get(h.server.URL + pathAndQuery)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// postJSON performs a JSON POST request.
func (h *e2eHarness) postJSON(t *testing.T, path string, payload any) *http.Response {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := h.server.Client().Post(
		h.server.URL+path,
		"application/json",
		bytes.NewReader(b),
	)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// postRaw posts a raw body with an explicit content type.
func (h *e2eHarness) postRaw(t *testing.T, path, contentType, payload string) *http.Response {
	t.Helper()
	resp, err := h.server.Client().Post(h.server.URL+path, contentType, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func assertStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Errorf("status: got %d, want %d\nbody: %s", resp.StatusCode, want, truncate(body, 500))
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

func assertBodyContains(t *testing.T, resp *http.Response, substr string) {
	t.Helper()
	body := readBody(t, resp)
	if !strings.Contains(body, substr) {
		t.Errorf("body missing %q\nfirst 500 chars: %s", substr, truncate([]byte(body), 500))
	}
}

func assertBodyNotContains(t *testing.T, resp *http.Response, substr string) {
	t.Helper()
	body := readBody(t, resp)
	if strings.Contains(body, substr) {
		t.Errorf("body unexpectedly contains %q", substr)
	}
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// ==========================================================================
// Main page
// ==========================================================================

func TestE2E_MainPageLoads(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?since=87600h")
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, "Log Explorer")
}

func TestE2E_MainPageRendersAllRows(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?since=87600h")
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	for _, s := range []string{
		"connection timeout to upstream",
		"request completed",
		"rate limit approaching",
		"token validation failed",
		"debug: parsed headers",
	} {
		if !strings.Contains(body, s) {
			t.Errorf("missing row: %q", s)
		}
	}
}

func TestE2E_MainPageEmptyState(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?service_name=nonexistent-svc")
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, "No logs found")
}

// ==========================================================================
// Filters
// ==========================================================================

func TestE2E_FilterByService(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?since=87600h&service_name=api-gateway")
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	if !strings.Contains(body, "connection timeout") {
		t.Error("missing api-gateway rows")
	}
	if strings.Contains(body, "token validation failed") {
		t.Error("unexpected auth-svc rows")
	}
}

func TestE2E_FilterBySeverity(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?since=87600h&severity=ERROR")
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	if !strings.Contains(body, "connection timeout") {
		t.Error("missing ERROR row")
	}
	if strings.Contains(body, "request completed") {
		t.Error("unexpected INFO row")
	}
}

func TestE2E_FilterByTraceID(t *testing.T) {
	h := newE2EHarness(t, false)
	// trace1 = hex("00112233445566778899aabbccddeeff") = 16-byte BLOB.
	// The compiler wraps with lower(hex(...)) so the filter value is lowercase hex.
	resp := h.get(t, "/logs?since=87600h&trace_id=00112233445566778899aabbccddeeff")
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	if !strings.Contains(body, "connection timeout") {
		t.Error("missing first row for trace1")
	}
	if !strings.Contains(body, "request completed") {
		t.Error("missing second row for trace1")
	}
}

func TestE2E_BodySubstring(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?since=87600h&body=timeout")
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	if !strings.Contains(body, "connection timeout") {
		t.Error("missing timeout row")
	}
	if strings.Contains(body, "request completed") {
		t.Error("unexpected non-timeout row")
	}
}

// ==========================================================================
// FTS full-text search
// ==========================================================================

func TestE2E_FTSSearchWithIndex(t *testing.T) {
	h := newE2EHarness(t, true)
	if !h.client.HasFTS() {
		t.Fatal("expected HasFTS=true")
	}
	resp := h.get(t, "/logs?since=87600h&search=timeout")
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, "connection timeout")
}

func TestE2E_FTSSearchWithoutIndexFallsBack(t *testing.T) {
	h := newE2EHarness(t, false)
	if h.client.HasFTS() {
		t.Fatal("expected HasFTS=false for fallback test")
	}
	resp := h.get(t, "/logs?since=87600h&search=timeout")
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, "connection timeout")
}

func TestE2E_HybridFTSAndStructured(t *testing.T) {
	h := newE2EHarness(t, true)
	resp := h.get(t, "/logs?since=87600h&search=failed&service_name=auth-svc")
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	if !strings.Contains(body, "token validation failed") {
		t.Error("hybrid query should match auth-svc failure")
	}
	if strings.Contains(body, "connection timeout") {
		t.Error("hybrid query should NOT match api-gateway timeout")
	}
}

func TestE2E_FTSInvalidSyntaxIsBadRequest(t *testing.T) {
	h := newE2EHarness(t, true)
	// Common FTS5 syntax mistakes must yield a friendly 400, not a 500.
	for _, bad := range []string{`"`, `(timeout`, `error:`, `AND`, `*`} {
		resp := h.get(t, "/logs?since=87600h&search="+url.QueryEscape(bad))
		assertStatus(t, resp, 400)
		assertBodyContains(t, resp, "full-text")
	}
}

func TestE2E_JSONFTSInvalidSyntaxIsBadRequest(t *testing.T) {
	h := newE2EHarness(t, true)
	resp := h.postJSON(t, "/logs/query", map[string]any{
		"where": map[string]any{"match": "AND"},
		"limit": 10,
	})
	assertStatus(t, resp, 400)
}

// ==========================================================================
// Detail page
// ==========================================================================

func TestE2E_DetailPageValidID(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs/1")
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	if !strings.Contains(body, "connection timeout to upstream") {
		t.Error("detail page missing body")
	}
	if !strings.Contains(body, "api-gateway") {
		t.Error("detail page missing service name")
	}
	if !strings.Contains(body, "component") || !strings.Contains(body, "gateway") {
		t.Error("detail page missing inline event attributes")
	}
}

func TestE2E_DetailPageInvalidID(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs/99999")
	assertStatus(t, resp, 404)
	body := readBody(t, resp)
	for _, s := range []string{"Not Found", "Back to logs"} {
		if !strings.Contains(body, s) {
			t.Errorf("error page missing %q", s)
		}
	}
}

func TestE2E_DetailPageNegativeID(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs/-1")
	assertStatus(t, resp, 400)
	body := readBody(t, resp)
	for _, s := range []string{"Bad Request", "positive integer"} {
		if !strings.Contains(body, s) {
			t.Errorf("error page missing %q", s)
		}
	}
}

func TestE2E_DetailPageNonNumericID(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs/abc")
	assertStatus(t, resp, 400)
	assertBodyContains(t, resp, "Bad Request")
}

// ==========================================================================
// Error page
// ==========================================================================

func TestE2E_UnknownRouteReturnsStyled404(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/nope")
	assertStatus(t, resp, 404)
	body := readBody(t, resp)
	for _, s := range []string{"Not Found", "Back to logs", "style.css"} {
		if !strings.Contains(body, s) {
			t.Errorf("error page missing %q", s)
		}
	}
}

func TestE2E_StaticStylesheetServed(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/static/style.css")
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	if !strings.Contains(body, "body {") || !strings.Contains(body, "error-card") {
		t.Errorf("stylesheet missing expected rules")
	}
}

// ==========================================================================
// JSON API
// ==========================================================================

func TestE2E_JSONQueryValid(t *testing.T) {
	h := newE2EHarness(t, false)
	payload := map[string]any{
		"where": map[string]any{"eq": []any{"severity", "ERROR"}},
		"sort":  []map[string]any{{"field": "timestamp", "desc": true}},
		"limit": 10,
	}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/logs/query", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	assertStatus(t, resp, 200)
	var rows []db.LogRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_ = resp.Body.Close()
	if len(rows) != 2 {
		t.Fatalf("expected 2 ERROR rows, got %d", len(rows))
	}
	for _, r := range rows {
		if r.Severity != "ERROR" {
			t.Errorf("expected ERROR, got %s", r.Severity)
		}
	}
}

func TestE2E_JSONQueryInvalidDSL(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.postJSON(t, "/logs/query", map[string]any{
		"where": map[string]any{"eq": []any{"nonexistent_field", "value"}},
		"limit": 10,
	})
	assertStatus(t, resp, 400)
}

func TestE2E_JSONQueryLimitExceeded(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.postJSON(t, "/logs/query", map[string]any{"limit": 2000})
	assertStatus(t, resp, 400)
}

func TestE2E_JSONQueryShorthandFormat(t *testing.T) {
	h := newE2EHarness(t, false)
	payload := map[string]any{
		"where": map[string]any{"contains": []any{"body", "timeout"}},
		"limit": 10,
	}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/logs/query", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	assertStatus(t, resp, 200)
	var rows []db.LogRow
	_ = json.NewDecoder(resp.Body).Decode(&rows)
	_ = resp.Body.Close()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
}

// ==========================================================================
// Query page results
// ==========================================================================

func TestE2E_QueryPageRows(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?service_name=api-gateway&limit=10")
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	if !strings.Contains(body, "<tr>") {
		t.Error("missing <tr>")
	}
	if !strings.Contains(body, "connection timeout") {
		t.Error("missing api-gateway row")
	}
	if strings.Contains(body, "token validation failed") {
		t.Error("unexpected auth-svc row")
	}
}

// ==========================================================================
// Pagination
// ==========================================================================

func TestE2E_PaginationFirstPage(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?since=87600h&limit=2&offset=0")
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, "Next")
}

func TestE2E_PaginationSecondPage(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?since=87600h&limit=2&offset=2")
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, "Prev")
}

func TestE2E_PaginationLastPageHasNoNext(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?since=87600h&limit=100&offset=0")
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, "disabled")
}

// ==========================================================================
// Error handling
// ==========================================================================

func TestE2E_InvalidLimit(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?limit=notanumber")
	assertStatus(t, resp, 200)
}

// ==========================================================================
// XSS safety
// ==========================================================================

func TestE2E_XSSInServiceName(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?service_name="+url.QueryEscape(`<script>alert(1)</script>`))
	assertStatus(t, resp, 200)
	assertBodyNotContains(t, resp, `<script>alert(1)</script>`)
}

func TestE2E_XSSInBody(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?body="+url.QueryEscape(`<img src=x onerror=alert(1)>`))
	assertStatus(t, resp, 200)
	assertBodyNotContains(t, resp, `<img src=x onerror=alert(1)>`)
}

// ==========================================================================
// Read-only enforcement
// ==========================================================================

func TestReadOnly_FileIntegrity(t *testing.T) {
	path := setupTestDB(t)
	t.Cleanup(func() { _ = os.Remove(path) })
	hashBefore := fileHash(t, path)

	client, err := db.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	var q dsl.Query
	q.Where = dsl.BinaryExpr{
		Op:    dsl.OpContains,
		Field: "service_name",
		Value: dsl.Value{Type: dsl.ValueString, String: "api"},
	}
	q.Limit = 100
	_ = dsl.Validate(&q)
	dsl.Normalize(&q)
	cq, _ := compiler.Compile(&q)
	for range 50 {
		rows, err := client.Execute(cq)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if len(rows) == 0 {
			t.Fatal("expected results")
		}
	}
	cqID := compiler.CompileGetByID(1)
	for range 25 {
		rows, err := client.Execute(cqID)
		if err != nil {
			t.Fatalf("execute by id: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(rows))
		}
	}
	for range 10 {
		_ = client.HasFTS()
	}
	_ = client.Close()

	hashAfter := fileHash(t, path)
	if hashBefore != hashAfter {
		t.Errorf("DATABASE FILE WAS MODIFIED\nbefore: %s\nafter:  %s", hashBefore, hashAfter)
	}
}

func TestReadOnly_WriteRejected(t *testing.T) {
	path := setupTestDB(t)
	t.Cleanup(func() { _ = os.Remove(path) })
	client, err := db.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = client.Close() }()

	_, err = client.DB().Exec("CREATE TABLE should_fail (x INTEGER)")
	if err == nil {
		t.Fatal("expected error for write on read-only connection")
	}
	_, err = client.DB().Exec("INSERT INTO logs (body) VALUES ('test')")
	if err == nil {
		t.Fatal("expected error for INSERT on read-only connection")
	}
	_, err = client.DB().Exec("DELETE FROM logs WHERE id = 1")
	if err == nil {
		t.Fatal("expected error for DELETE on read-only connection")
	}
}

func TestReadOnly_FileIntegrityWithFTS(t *testing.T) {
	path := setupTestDBWithFTS(t)
	t.Cleanup(func() { _ = os.Remove(path) })
	hashBefore := fileHash(t, path)

	client, err := db.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !client.HasFTS() {
		t.Fatal("expected HasFTS=true")
	}
	var q dsl.Query
	q.Where = dsl.MatchExpr{Query: "timeout"}
	q.Limit = 100
	_ = dsl.Validate(&q)
	dsl.Normalize(&q)
	cq, _ := compiler.Compile(&q)
	for range 50 {
		rows, err := client.Execute(cq)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(rows))
		}
	}
	_ = client.Close()

	hashAfter := fileHash(t, path)
	if hashBefore != hashAfter {
		t.Errorf("DATABASE FILE WAS MODIFIED\nbefore: %s\nafter:  %s", hashBefore, hashAfter)
	}
}

// ==========================================================================
// Concurrency
// ==========================================================================

func TestE2E_ConcurrentRequests(t *testing.T) {
	h := newE2EHarness(t, false)
	const (
		concurrency = 10
		iterations  = 20
	)
	errs := make(chan error, concurrency)
	for i := range concurrency {
		go func(id int) {
			for _, path := range []string{
				"/logs?since=87600h&limit=10",
				"/logs/1",
				"/logs?service_name=api-gateway",
			} {
				for range iterations {
					status, err := h.getStatus(path)
					if err != nil {
						errs <- fmt.Errorf("g%d: GET %s: %w", id, path, err)
						return
					}
					if status != http.StatusOK {
						errs <- fmt.Errorf("g%d: GET %s returned %d", id, path, status)
						return
					}
				}
			}
			errs <- nil
		}(i)
	}
	for range concurrency {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

// ==========================================================================
// Edge cases
// ==========================================================================

func TestE2E_MaxLimitAllRows(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?since=87600h&limit=1000")
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	count := strings.Count(body, "api-gateway") + strings.Count(body, "auth-svc")
	if count < 5 {
		t.Errorf("expected at least 5 row occurrences, got %d", count)
	}
}

func TestE2E_ZeroLimit(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?since=87600h&limit=0")
	assertStatus(t, resp, 200)
}

func TestE2E_EmptySearchDoesNotError(t *testing.T) {
	h := newE2EHarness(t, true)
	resp := h.get(t, "/logs?since=87600h&search=")
	assertStatus(t, resp, 200)
	_ = readBody(t, resp)
}

func TestE2E_DateFromFilter(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?since=87600h&date_from=2099-01-01T00:00")
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, "No logs found")
}

func TestE2E_SinceFilter(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?since=1s")
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, "No logs found")
}

func TestE2E_SortByServiceAsc(t *testing.T) {
	h := newE2EHarness(t, false)
	payload := map[string]any{
		"sort":  []map[string]any{{"field": "service_name", "desc": false}},
		"limit": 10,
	}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/logs/query", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	assertStatus(t, resp, 200)
	var rows []db.LogRow
	_ = json.NewDecoder(resp.Body).Decode(&rows)
	_ = resp.Body.Close()
	if len(rows) < 2 {
		t.Fatal("expected at least 2 rows")
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].ServiceName > rows[i].ServiceName {
			t.Errorf("not sorted ascending: %s > %s at idx %d",
				rows[i-1].ServiceName, rows[i].ServiceName, i)
		}
	}
}

// ==========================================================================
// Helpers
// ==========================================================================

func fileHash(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// ==========================================================================
// Hardening regressions
// ==========================================================================

func TestE2E_Healthz(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/healthz")
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	if !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("unexpected health body: %q", body)
	}
}

func TestE2E_SecurityHeaders(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?since=87600h")
	assertStatus(t, resp, 200)
	_ = readBody(t, resp)
	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "SAMEORIGIN",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("header %s: got %q, want %q", k, got, want)
		}
	}
}

func TestE2E_TraceIDUppercase(t *testing.T) {
	h := newE2EHarness(t, false)
	// Uppercase hex must match the lower(hex(...)) expression.
	resp := h.get(t, "/logs?since=87600h&trace_id=00112233445566778899AABBCCDDEEFF")
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	if !strings.Contains(body, "connection timeout") {
		t.Error("uppercase trace_id filter matched nothing")
	}
}

func TestE2E_UnknownSeverityIsBadRequest(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?since=87600h&severity=BOGUS")
	assertStatus(t, resp, 400)
	assertBodyContains(t, resp, "unknown severity")
}

func TestE2E_InvalidDateIsBadRequest(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?date_from=not-a-date")
	assertStatus(t, resp, 400)
}

func TestE2E_InvalidSinceIsBadRequest(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?since=tomorrow")
	assertStatus(t, resp, 400)
}

func TestE2E_SinceDaysWorks(t *testing.T) {
	h := newE2EHarness(t, false)
	// 36500d ≈ 100y covers the 2023 fixture rows.
	resp := h.get(t, "/logs?since=36500d")
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, "connection timeout")
}

func TestE2E_SinceSelectionPreserved(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?since=24h")
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, `<option value="24h" selected>`)
}

func TestE2E_CustomSinceSelectionPreserved(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.get(t, "/logs?since=48h")
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, `<option value="48h" selected>`)
}

func TestE2E_ContainsWildcardIsLiteral(t *testing.T) {
	h := newE2EHarness(t, false)
	// "%" must be matched literally (no rows), not as a LIKE wildcard that
	// matches everything — and the ESCAPE clause must be valid SQLite.
	resp := h.get(t, "/logs?since=87600h&body="+url.QueryEscape("%"))
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, "No logs found")
}

func TestE2E_JSONEmptyResultIsArray(t *testing.T) {
	h := newE2EHarness(t, false)
	payload := map[string]any{
		"where": map[string]any{"eq": []any{"service_name", "no-such-service"}},
		"limit": 10,
	}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/logs/query", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	assertStatus(t, resp, 200)
	// Empty results must encode as [] so clients can unmarshal unconditionally.
	if body := strings.TrimSpace(readBody(t, resp)); body != "[]" {
		t.Errorf("expected empty JSON array, got %q", body)
	}
}

func TestE2E_JSONRejectsUnknownField(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.postJSON(t, "/logs/query", map[string]any{"limt": 10})
	assertStatus(t, resp, 400)
}

func TestE2E_JSONBodyTooLargeIsRejected(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.postJSON(t, "/logs/query", map[string]any{
		"where": map[string]any{"contains": []any{"body", strings.Repeat("a", 2<<20)}},
		"limit": 10,
	})
	assertStatus(t, resp, 413)
}

func TestE2E_JSONTrailingDataIsRejected(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.postRaw(t, "/logs/query", "application/json", `{"limit":1} trailing`)
	assertStatus(t, resp, 400)
}

func TestE2E_JSONUnknownSortFieldIsRejected(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.postRaw(t, "/logs/query", "application/json",
		`{"sort":[{"field":"timestamp","desc":true,"typo":1}],"limit":1}`)
	assertStatus(t, resp, 400)
}

func TestE2E_JSONNullWhereIsAccepted(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.postRaw(t, "/logs/query", "application/json", `{"where":null,"limit":1}`)
	assertStatus(t, resp, 200)
}

func TestE2E_JSONContentTypeMustBeExact(t *testing.T) {
	h := newE2EHarness(t, false)
	resp := h.postRaw(t, "/logs/query", "text/application/json", `{"limit":1}`)
	assertStatus(t, resp, 415)
}
