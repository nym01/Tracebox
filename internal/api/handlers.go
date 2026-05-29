package api

import (
	"encoding/json"
	"net/http"

	_ "github.com/nym01/goboxd/internal/language"
)

func RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("POST /run", run)
}

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func run(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		return
	}
	if verr := validateRunRequest(&req); verr != nil {
		writeError(w, http.StatusBadRequest, verr.Code, verr.Message)
		return
	}
	// TODO: execute run (Step 3)
	w.WriteHeader(http.StatusNotImplemented)
}
