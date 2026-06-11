package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nym01/goboxd/internal/language"
	"github.com/nym01/goboxd/internal/runner"
)

func getInfo(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/info", nil)
	w := httptest.NewRecorder()
	infoHandler(w, req)
	return w
}

func TestInfoReturns200(t *testing.T) {
	w := getInfo(t)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
}

func TestInfoResponseShape(t *testing.T) {
	w := getInfo(t)

	var resp infoResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.BuildInfo.Version == "" {
		t.Error("build_info.version must be non-empty")
	}
	if resp.BuildInfo.Commit == "" {
		t.Error("build_info.commit must be non-empty")
	}
	if resp.BuildInfo.GoVersion == "" {
		t.Error("build_info.go_version must be non-empty")
	}
	if resp.Limits.MaxSourceBytes != maxSourceBytes {
		t.Errorf("limits.max_source_bytes: want %d, got %d", maxSourceBytes, resp.Limits.MaxSourceBytes)
	}
	if resp.Limits.MaxTests != maxTests {
		t.Errorf("limits.max_tests: want %d, got %d", maxTests, resp.Limits.MaxTests)
	}
}

func TestInfoContainsAllLanguages(t *testing.T) {
	w := getInfo(t)

	var resp infoResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	registered := language.All()
	if len(resp.Languages) != len(registered) {
		t.Errorf("language count: want %d, got %d", len(registered), len(resp.Languages))
	}

	seen := make(map[string]bool, len(resp.Languages))
	for _, l := range resp.Languages {
		seen[l.ID] = true
	}
	for _, l := range registered {
		if !seen[l.ID] {
			t.Errorf("language %q missing from /info response", l.ID)
		}
	}
}

func TestInfoJobsTotalIncrements(t *testing.T) {
	orig := defaultRunner
	defaultRunner = &fakeRunner{result: runner.RunResult{Stdout: "hi\n", ExitCode: 0}}
	defer func() { defaultRunner = orig }()

	before := jobsTotal.Load()

	body := `{"language":"py3","source":"print('hi')","tests":[{"stdin":"","expected_stdout":"hi\n"}]}`
	postRun(t, body)

	after := jobsTotal.Load()
	if after != before+1 {
		t.Errorf("jobs_total: want %d, got %d", before+1, after)
	}
}

func TestInfoJobsTotalReflectedInResponse(t *testing.T) {
	orig := defaultRunner
	defaultRunner = &fakeRunner{result: runner.RunResult{Stdout: "hi\n", ExitCode: 0}}
	defer func() { defaultRunner = orig }()

	before := getInfoJobsTotal(t)

	body := `{"language":"py3","source":"print('hi')","tests":[{"stdin":"","expected_stdout":"hi\n"}]}`
	postRun(t, body)
	postRun(t, body)

	after := getInfoJobsTotal(t)
	if after != before+2 {
		t.Errorf("jobs_total in /info response: want %d, got %d", before+2, after)
	}
}

func getInfoJobsTotal(t *testing.T) int64 {
	t.Helper()
	w := getInfo(t)
	var resp infoResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Stats.JobsTotal
}
