package api

import (
	"net/http"
	"os"
)

// corsOrigin returns the value for the Access-Control-Allow-Origin header.
// It is configurable via TRACEBOX_CORS_ORIGIN and defaults to "*" so a
// browser-based frontend can call the API during local development.
func corsOrigin() string {
	if v := os.Getenv("TRACEBOX_CORS_ORIGIN"); v != "" {
		return v
	}
	return "*"
}

// WithCORS wraps an http.Handler with the CORS headers needed for JSON POST
// requests from a browser, and short-circuits OPTIONS preflight requests with
// a 204 response. It is purely an HTTP-layer addition and does not touch the
// runner, request validation, or response bodies.
//
// It wraps the whole mux rather than individual handlers so that OPTIONS
// preflight requests are answered before the method-based ServeMux routing
// (e.g. "POST /run") rejects them as 404.
func WithCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", corsOrigin())
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
		// Vary so caches don't serve a response with the wrong origin when
		// TRACEBOX_CORS_ORIGIN is set to a specific value.
		h.Add("Vary", "Origin")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
