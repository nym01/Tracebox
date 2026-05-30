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

// sequencedRunner returns results in order; the last result repeats for extra calls.
type sequencedRunner struct {
	results []runner.RunResult
	n       int
}

func (s *sequencedRunner) Run(_ context.Context, _ runner.RunSpec) (runner.RunResult, error) {
	i := s.n
	if i >= len(s.results) {
		i = len(s.results) - 1
	}
	s.n++
	return s.results[i], nil
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

func TestCppRuntimeError(t *testing.T) {
	orig := defaultRunner
	defaultRunner = &sequencedRunner{results: []runner.RunResult{
		{ExitCode: 0},                               // build ok
		{ExitCode: 1, Stderr: "Segmentation fault"}, // run exits non-zero
	}}
	defer func() { defaultRunner = orig }()

	body := `{"language":"cpp","source":"#include <cstdlib>\nint main(){return 1;}","source_filename":"solution.cpp","artifact_filename":"solution","tests":[{"stdin":"","expected_stdout":""}]}`
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
	if resp.Build == nil || resp.Build.Status != "ok" {
		t.Errorf("build.status: want ok, got %v", resp.Build)
	}
	if len(resp.Tests) != 1 {
		t.Fatalf("want 1 test result, got %d", len(resp.Tests))
	}
	if resp.Tests[0].Status != "runtime_error" {
		t.Errorf("tests[0].status: want runtime_error, got %q", resp.Tests[0].Status)
	}
}

func TestCppTimeExceeded(t *testing.T) {
	orig := defaultRunner
	defaultRunner = &sequencedRunner{results: []runner.RunResult{
		{ExitCode: 0},                  // build ok
		{TimedOut: true, ExitCode: -1}, // run times out
	}}
	defer func() { defaultRunner = orig }()

	body := `{"language":"cpp","source":"int main(){while(true){}}","source_filename":"solution.cpp","artifact_filename":"solution","tests":[{"stdin":"","expected_stdout":""}]}`
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
	if resp.Build == nil || resp.Build.Status != "ok" {
		t.Errorf("build.status: want ok, got %v", resp.Build)
	}
	if len(resp.Tests) != 1 {
		t.Fatalf("want 1 test result, got %d", len(resp.Tests))
	}
	if resp.Tests[0].Status != "time_exceeded" {
		t.Errorf("tests[0].status: want time_exceeded, got %q", resp.Tests[0].Status)
	}
}

func TestCppWrongOutput(t *testing.T) {
	orig := defaultRunner
	defaultRunner = &sequencedRunner{results: []runner.RunResult{
		{ExitCode: 0},               // build ok
		{Stdout: "wrong\n", ExitCode: 0}, // run prints wrong answer
	}}
	defer func() { defaultRunner = orig }()

	body := `{"language":"cpp","source":"#include<iostream>\nint main(){std::cout<<\"wrong\\n\";}","source_filename":"solution.cpp","artifact_filename":"solution","tests":[{"stdin":"","expected_stdout":"right\n"}]}`
	w := postRun(t, body)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}
	var resp RunResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "wrong_output" {
		t.Errorf("top-level status: want wrong_output, got %q", resp.Status)
	}
	if resp.Build == nil || resp.Build.Status != "ok" {
		t.Errorf("build.status: want ok, got %v", resp.Build)
	}
	if len(resp.Tests) != 1 {
		t.Fatalf("want 1 test result, got %d", len(resp.Tests))
	}
	if resp.Tests[0].Status != "wrong_output" {
		t.Errorf("tests[0].status: want wrong_output, got %q", resp.Tests[0].Status)
	}
}

func TestCppOutputWhitespaceMismatch(t *testing.T) {
	orig := defaultRunner
	defaultRunner = &sequencedRunner{results: []runner.RunResult{
		{ExitCode: 0},                       // build ok
		{Stdout: "hello   \n", ExitCode: 0}, // trailing spaces
	}}
	defer func() { defaultRunner = orig }()

	body := `{"language":"cpp","source":"#include<iostream>\nint main(){std::cout<<\"hello   \\n\";}","source_filename":"solution.cpp","artifact_filename":"solution","tests":[{"stdin":"","expected_stdout":"hello\n"}]}`
	w := postRun(t, body)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}
	var resp RunResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "output_whitespace_mismatch" {
		t.Errorf("top-level status: want output_whitespace_mismatch, got %q", resp.Status)
	}
	if resp.Build == nil || resp.Build.Status != "ok" {
		t.Errorf("build.status: want ok, got %v", resp.Build)
	}
	if len(resp.Tests) != 1 {
		t.Fatalf("want 1 test result, got %d", len(resp.Tests))
	}
	if resp.Tests[0].Status != "output_whitespace_mismatch" {
		t.Errorf("tests[0].status: want output_whitespace_mismatch, got %q", resp.Tests[0].Status)
	}
}

// capturingRunner records every RunSpec it receives and delegates to sequenced results.
type capturingRunner struct {
	results []runner.RunResult
	specs   []runner.RunSpec
	n       int
}

func (c *capturingRunner) Run(_ context.Context, spec runner.RunSpec) (runner.RunResult, error) {
	c.specs = append(c.specs, spec)
	i := c.n
	if i >= len(c.results) {
		i = len(c.results) - 1
	}
	c.n++
	return c.results[i], nil
}

func TestPythonStdinSingleLine(t *testing.T) {
	orig := defaultRunner
	cap := &capturingRunner{results: []runner.RunResult{{Stdout: "hello\n", ExitCode: 0}}}
	defaultRunner = cap
	defer func() { defaultRunner = orig }()

	body := `{"language":"py3","source":"line=input();print(line)","tests":[{"stdin":"hello","expected_stdout":"hello\n"}]}`
	w := postRun(t, body)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}
	if len(cap.specs) != 1 {
		t.Fatalf("expected 1 Run call, got %d", len(cap.specs))
	}
	if cap.specs[0].Stdin != "hello" {
		t.Errorf("stdin piped to runner: want %q, got %q", "hello", cap.specs[0].Stdin)
	}
	var resp RunResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "accepted" {
		t.Errorf("top-level status: want accepted, got %q", resp.Status)
	}
}

func TestPythonStdinMultiline(t *testing.T) {
	const stdinVal = "line1\nline2\nline3\n"
	orig := defaultRunner
	cap := &capturingRunner{results: []runner.RunResult{{Stdout: stdinVal, ExitCode: 0}}}
	defaultRunner = cap
	defer func() { defaultRunner = orig }()

	body := `{"language":"py3","source":"import sys\nprint(sys.stdin.read(),end='')","tests":[{"stdin":"line1\nline2\nline3\n","expected_stdout":"line1\nline2\nline3\n"}]}`
	w := postRun(t, body)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}
	if len(cap.specs) != 1 {
		t.Fatalf("expected 1 Run call, got %d", len(cap.specs))
	}
	if cap.specs[0].Stdin != stdinVal {
		t.Errorf("stdin piped to runner: want %q, got %q", stdinVal, cap.specs[0].Stdin)
	}
	var resp RunResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "accepted" {
		t.Errorf("top-level status: want accepted, got %q", resp.Status)
	}
}

func TestCppStdinSingleLine(t *testing.T) {
	orig := defaultRunner
	cap := &capturingRunner{results: []runner.RunResult{
		{ExitCode: 0},                    // build
		{Stdout: "hello\n", ExitCode: 0}, // run
	}}
	defaultRunner = cap
	defer func() { defaultRunner = orig }()

	body := `{"language":"cpp","source":"#include<iostream>\n#include<string>\nint main(){std::string s;std::getline(std::cin,s);std::cout<<s<<\"\\n\";}","source_filename":"solution.cpp","artifact_filename":"solution","tests":[{"stdin":"hello","expected_stdout":"hello\n"}]}`
	w := postRun(t, body)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}
	if len(cap.specs) != 2 {
		t.Fatalf("expected 2 Run calls (build+run), got %d", len(cap.specs))
	}
	if cap.specs[0].Stdin != "" {
		t.Errorf("build stdin: want empty, got %q", cap.specs[0].Stdin)
	}
	if cap.specs[1].Stdin != "hello" {
		t.Errorf("run stdin piped to runner: want %q, got %q", "hello", cap.specs[1].Stdin)
	}
	var resp RunResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "accepted" {
		t.Errorf("top-level status: want accepted, got %q", resp.Status)
	}
}

func TestCppStdinMultiline(t *testing.T) {
	const stdinVal = "3\n10\n20\n30\n"
	orig := defaultRunner
	cap := &capturingRunner{results: []runner.RunResult{
		{ExitCode: 0},                // build
		{Stdout: "60\n", ExitCode: 0}, // run
	}}
	defaultRunner = cap
	defer func() { defaultRunner = orig }()

	body := `{"language":"cpp","source":"#include<iostream>\nint main(){int n,s=0,x;std::cin>>n;for(int i=0;i<n;i++){std::cin>>x;s+=x;}std::cout<<s<<\"\\n\";}","source_filename":"solution.cpp","artifact_filename":"solution","tests":[{"stdin":"3\n10\n20\n30\n","expected_stdout":"60\n"}]}`
	w := postRun(t, body)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}
	if len(cap.specs) != 2 {
		t.Fatalf("expected 2 Run calls (build+run), got %d", len(cap.specs))
	}
	if cap.specs[0].Stdin != "" {
		t.Errorf("build stdin: want empty, got %q", cap.specs[0].Stdin)
	}
	if cap.specs[1].Stdin != stdinVal {
		t.Errorf("run stdin piped to runner: want %q, got %q", stdinVal, cap.specs[1].Stdin)
	}
	var resp RunResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "accepted" {
		t.Errorf("top-level status: want accepted, got %q", resp.Status)
	}
}

// TestCppBuildFieldPresent verifies that a compiled language (cpp) always
// includes the build field with all required subfields in the response.
func TestCppBuildFieldPresent(t *testing.T) {
	orig := defaultRunner
	defaultRunner = &sequencedRunner{results: []runner.RunResult{
		{ExitCode: 0, Stdout: "", Stderr: "note: optimization", DurationMs: 412}, // build ok
		{ExitCode: 0, Stdout: "hi\n"},                                            // run ok
	}}
	defer func() { defaultRunner = orig }()

	body := `{"language":"cpp","source":"#include<iostream>\nint main(){std::cout<<\"hi\\n\";}","source_filename":"solution.cpp","artifact_filename":"solution","tests":[{"stdin":"","expected_stdout":"hi\n"}]}`
	w := postRun(t, body)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}

	// Decode into raw map to confirm "build" key is present in JSON.
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(w.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	buildRaw, ok := raw["build"]
	if !ok {
		t.Fatal("build field must be present for compiled language cpp, but it was absent")
	}

	// Confirm all required subfields exist with correct types.
	var build BuildResult
	if err := json.Unmarshal(buildRaw, &build); err != nil {
		t.Fatalf("unmarshal build: %v", err)
	}
	if build.Status == "" {
		t.Error("build.status must be non-empty")
	}
	if build.Status != "ok" {
		t.Errorf("build.status: want ok, got %q", build.Status)
	}
	// stdout and stderr are valid as empty strings; just confirm the fields round-trip.
	var buildMap map[string]json.RawMessage
	if err := json.Unmarshal(buildRaw, &buildMap); err != nil {
		t.Fatalf("unmarshal build map: %v", err)
	}
	for _, field := range []string{"status", "stdout", "stderr", "duration_ms"} {
		if _, present := buildMap[field]; !present {
			t.Errorf("build.%s field is missing from JSON", field)
		}
	}
}

// TestPy3BuildFieldAbsent verifies that an interpreted language (py3) never
// includes the build field in the response.
func TestPy3BuildFieldAbsent(t *testing.T) {
	orig := defaultRunner
	defaultRunner = &fakeRunner{result: runner.RunResult{Stdout: "hi\n", ExitCode: 0}}
	defer func() { defaultRunner = orig }()

	body := `{"language":"py3","source":"print('hi')","tests":[{"stdin":"","expected_stdout":"hi\n"}]}`
	w := postRun(t, body)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}

	// Decode into raw map to confirm "build" key is absent from JSON.
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(w.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["build"]; ok {
		t.Fatal("build field must be absent for interpreted language py3, but it was present")
	}

	// Sanity: top-level status is accepted.
	var statusVal string
	if err := json.Unmarshal(raw["status"], &statusVal); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if statusVal != "accepted" {
		t.Errorf("top-level status: want accepted, got %q", statusVal)
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
