package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandlerRecoversFromPanic verifies a panicking handler yields a 500
// instead of killing the connection, with a clean error body.
func TestHandlerRecoversFromPanic(t *testing.T) {
	srv := &Server{}
	h := srv.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("got status %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Internal server error") {
		t.Errorf("unexpected body: %q", rec.Body.String())
	}
}
