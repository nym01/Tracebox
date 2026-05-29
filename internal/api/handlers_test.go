package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nym01/goboxd/internal/runner"
)

type fakeRunner struct {
	result runner.RunResult
	err    error
}

func (f *fakeRunner) Run(_ context.Context, _ runner.RunSpec) (runner.RunResult, error) {
	return f.result, f.err
}

func postRun(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	run(w, req)
	return w
}

func TestRuntimeError(t *testing.T) {
	orig := defaultRunner
	defaultRunner = &fakeRunner{result: runner.RunResult{ExitCode: 1, Stderr: "traceback"}}
	defer func() { defaultRunner = orig }()

	body := `{"language":"py3","source":"import sys; sys.exit(1)","tests":[{"stdin":"","expected_stdout":""}]}`
	w := postRun(t, body)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}
	var resp RunResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "runtime_error" {
		t.Errorf("top-level status: want runtime_error, got %q", resp.Status)
	}
	if len(resp.Tests) != 1 {
		t.Fatalf("want 1 test result, got %d", len(resp.Tests))
	}
	if resp.Tests[0].Status != "runtime_error" {
		t.Errorf("tests[0].status: want runtime_error, got %q", resp.Tests[0].Status)
	}
}

func TestTimeExceeded(t *testing.T) {
	orig := defaultRunner
	defaultRunner = &fakeRunner{result: runner.RunResult{TimedOut: true, ExitCode: -1}}
	defer func() { defaultRunner = orig }()

	body := `{"language":"py3","source":"while True: pass","tests":[{"stdin":"","expected_stdout":""}]}`
	w := postRun(t, body)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}
	var resp RunResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "time_exceeded" {
		t.Errorf("top-level status: want time_exceeded, got %q", resp.Status)
	}
	if len(resp.Tests) != 1 {
		t.Fatalf("want 1 test result, got %d", len(resp.Tests))
	}
	if resp.Tests[0].Status != "time_exceeded" {
		t.Errorf("tests[0].status: want time_exceeded, got %q", resp.Tests[0].Status)
	}
}

func TestBuildFailed(t *testing.T) {
	orig := defaultRunner
	defaultRunner = &fakeRunner{result: runner.RunResult{ExitCode: 1, Stderr: "error: expected ';'"}}
	defer func() { defaultRunner = orig }()

	body := `{"language":"cpp","source":"int main(){","source_filename":"solution.cpp","artifact_filename":"solution","tests":[{"stdin":"","expected_stdout":"hi\n"}]}`
	w := postRun(t, body)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}
	var resp RunResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "build_failed" {
		t.Errorf("top-level status: want build_failed, got %q", resp.Status)
	}
	if resp.Build == nil {
		t.Fatal("expected build field in response, got nil")
	}
	if resp.Build.Status != "failed" {
		t.Errorf("build.status: want failed, got %q", resp.Build.Status)
	}
	if len(resp.Tests) != 1 {
		t.Fatalf("want 1 test result, got %d", len(resp.Tests))
	}
	if resp.Tests[0].Status != "not_executed" {
		t.Errorf("tests[0].status: want not_executed, got %q", resp.Tests[0].Status)
	}
}

// TestTopLevelFirstNonAccepted verifies that when test 1 passes and test 2
// fails, the top-level status is the second test's status (first non-accepted).
func TestTopLevelFirstNonAccepted(t *testing.T) {
	orig := defaultRunner
	// Runner always produces "hello\n"; test 1 expects it (accepted),
	// test 2 expects something different (wrong_output).
	defaultRunner = &fakeRunner{result: runner.RunResult{Stdout: "hello\n", ExitCode: 0}}
	defer func() { defaultRunner = orig }()

	body := `{"language":"py3","source":"print('hello')","tests":[{"stdin":"","expected_stdout":"hello\n"},{"stdin":"","expected_stdout":"world\n"}]}`
	w := postRun(t, body)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}
	var resp RunResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Tests) != 2 {
		t.Fatalf("want 2 test results, got %d", len(resp.Tests))
	}
	if resp.Tests[0].Status != "accepted" {
		t.Errorf("tests[0].status: want accepted, got %q", resp.Tests[0].Status)
	}
	if resp.Tests[1].Status != "wrong_output" {
		t.Errorf("tests[1].status: want wrong_output, got %q", resp.Tests[1].Status)
	}
	if resp.Status != "wrong_output" {
		t.Errorf("top-level status: want wrong_output, got %q", resp.Status)
	}
}
