package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/nym01/goboxd/internal/store"
)

// withTestStore installs a fresh file-backed store for the test and restores the
// previous (nil) store afterwards.
func withTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	prev := runStore
	SetStore(s)
	t.Cleanup(func() {
		SetStore(prev)
		s.Close()
	})
	return s
}

func TestGetRunHandler(t *testing.T) {
	s := withTestStore(t)
	run := store.RunRecord{
		RunID: "01890000-0000-7000-8000-00000000abcd", Language: "py3",
		Status: "accepted", Source: "print('hi')", Stdout: "hi\n", Timestamp: "t",
	}
	events := []store.TraceEventRecord{
		{Event: "connect", Syscall: "connect", DestIP: "8.8.8.8", DestPort: 53, Timestamp: "t1"},
	}
	if err := s.SaveRun(context.Background(), run, events); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	mux := http.NewServeMux()
	RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/runs/"+run.RunID, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp runDetailResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Source != "print('hi')" || resp.Stdout != "hi\n" {
		t.Errorf("audit fields missing: %+v", resp.RunRecord)
	}
	if len(resp.TraceEvents) != 1 || resp.TraceEvents[0].DestIP != "8.8.8.8" {
		t.Errorf("trace events not returned: %+v", resp.TraceEvents)
	}
}

func TestGetRunHandlerNotFound(t *testing.T) {
	withTestStore(t)
	mux := http.NewServeMux()
	RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/runs/01890000-0000-7000-8000-doesnotexist", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	var resp errorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error.Code != "run_not_found" {
		t.Errorf("expected code run_not_found, got %q", resp.Error.Code)
	}
}

func TestListRunsHandler(t *testing.T) {
	s := withTestStore(t)
	for _, id := range []string{"r1", "r2"} {
		if err := s.SaveRun(context.Background(), store.RunRecord{RunID: id, Language: "py3", Status: "accepted", Timestamp: "t"}, nil); err != nil {
			t.Fatalf("SaveRun: %v", err)
		}
	}
	mux := http.NewServeMux()
	RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/runs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp runsListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Runs) != 2 {
		t.Errorf("expected 2 runs, got %d", len(resp.Runs))
	}
}
