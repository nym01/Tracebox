package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func corsTestHandler() http.Handler {
	mux := http.NewServeMux()
	RegisterRoutes(mux)
	return WithCORS(mux)
}

func TestCORSPreflightOnRun(t *testing.T) {
	req := httptest.NewRequest(http.MethodOptions, "/run", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()

	corsTestHandler().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want %d", w.Code, http.StatusNoContent)
	}
	h := w.Header()
	if got := h.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q, want %q", got, "*")
	}
	if got := h.Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("Allow-Methods header missing")
	}
	if got := h.Get("Access-Control-Allow-Headers"); got == "" {
		t.Error("Allow-Headers header missing")
	}
}

func TestCORSConfigurableOrigin(t *testing.T) {
	t.Setenv("TRACEBOX_CORS_ORIGIN", "https://app.example.com")

	req := httptest.NewRequest(http.MethodOptions, "/run", nil)
	w := httptest.NewRecorder()
	corsTestHandler().ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("Allow-Origin = %q, want %q", got, "https://app.example.com")
	}
}

func TestCORSHeadersOnGet(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	corsTestHandler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want %d", w.Code, http.StatusOK)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q, want %q", got, "*")
	}
}
