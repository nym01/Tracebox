//go:build escapetests

// Package escapetests is the Phase 3 escape-test suite. Unlike the in-process
// integration tests (which call the api handler directly via httptest), these
// tests are a black-box HTTP client: they submit real /run requests over the
// network to a live, sandboxed container (Docker + nsjail), exactly as an
// attacker submitting untrusted code would. Each test ships a small program
// that attempts one specific escape and asserts on what the sandbox actually
// did with it.
//
// Run with:
//
//	docker build -t tracebox .
//	docker run --privileged --cgroupns=host --rm -d -p 8080:8080 \
//	    -e GOBOXD_RUNNER=nsjail tracebox
//	go test -tags escapetests -v ./escapetests/...
//
// The target URL defaults to http://127.0.0.1:8080 and can be overridden with
// the GOBOXD_ESCAPE_URL environment variable.
package escapetests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// baseURL returns the live container's base URL, overridable via env.
func baseURL() string {
	if u := os.Getenv("GOBOXD_ESCAPE_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://127.0.0.1:8080"
}

// testCase mirrors the API's per-test stdin/expected_stdout pair. For escape
// tests the verdict (accepted/wrong_output) is mostly irrelevant — we assert on
// the captured stdout/stderr of the attempt itself — so expected_stdout is
// usually left empty.
type testCase struct {
	Stdin          string `json:"stdin"`
	ExpectedStdout string `json:"expected_stdout"`
}

// runRequest is the subset of the /run request body these tests need.
type runRequest struct {
	Language         string     `json:"language"`
	Source           string     `json:"source"`
	SourceFilename   string     `json:"source_filename,omitempty"`
	ArtifactFilename string     `json:"artifact_filename,omitempty"`
	Tests            []testCase `json:"tests"`
}

type buildResult struct {
	Status string `json:"status"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

type testResult struct {
	Status       string `json:"status"`
	Stdout       string `json:"stdout"`
	Stderr       string `json:"stderr"`
	DurationMs   int64  `json:"duration_ms"`
	MemoryPeakKB int64  `json:"memory_peak_kb"`
}

type runResponse struct {
	Status string       `json:"status"`
	Build  *buildResult `json:"build,omitempty"`
	Tests  []testResult `json:"tests"`
}

// submit POSTs a /run request to the live container and returns the decoded
// response. It fails the test (not skips) if the container is unreachable: the
// suite is meaningless without a sandbox to attack, so an unreachable target is
// a real failure to surface, not a condition to paper over.
func submit(t *testing.T, req runRequest) runResponse {
	t.Helper()

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Post(baseURL()+"/run", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /run failed (is the sandboxed container running on %s?): %v", baseURL(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var sb strings.Builder
		_ = json.NewDecoder(resp.Body).Decode(&struct{}{})
		t.Fatalf("POST /run returned HTTP %d: %s", resp.StatusCode, sb.String())
	}

	var out runResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

// runPy3 submits a single-test py3 program and returns the first test's result.
// py3 is the simplest proven runtime, so it is the default vehicle for escape
// attempts unless a specific test needs a different language.
func runPy3(t *testing.T, source string) testResult {
	t.Helper()
	resp := submit(t, runRequest{
		Language: "py3",
		Source:   source,
		Tests:    []testCase{{Stdin: "", ExpectedStdout: ""}},
	})
	if len(resp.Tests) != 1 {
		t.Fatalf("expected exactly 1 test result, got %d (top status %q)", len(resp.Tests), resp.Status)
	}
	return resp.Tests[0]
}

// runJava compiles and runs a single-test Java program and returns the FULL
// response. java is one of the two languages the cgroup memory group exercises
// (notes pick it specifically because --rlimit_as could not contain the JVM in
// Phase 1, so the cgroup memory.max limit is what actually holds it). java's
// registry entry takes the source and artifact filenames from the request, so the
// public class name and these two fields must agree — callers pass e.g. class
// "Main" with className "Main" so the file is Main.java and the artifact Main.
func runJava(t *testing.T, source, className, expectedStdout string) runResponse {
	t.Helper()
	resp := submit(t, runRequest{
		Language:         "java",
		Source:           source,
		SourceFilename:   className + ".java",
		ArtifactFilename: className,
		Tests:            []testCase{{Stdin: "", ExpectedStdout: expectedStdout}},
	})
	if resp.Build == nil {
		t.Fatalf("java run returned no build result (top status %q)", resp.Status)
	}
	if len(resp.Tests) != 1 {
		t.Fatalf("expected exactly 1 test result, got %d (top status %q)", len(resp.Tests), resp.Status)
	}
	return resp
}

// runC compiles and runs a single-test C program and returns the FULL response,
// not just the run result. The seccomp tests need C — it is the only one of the
// seven runtimes that can issue a raw syscall (ptrace, unshare, umount2, setns,
// fork) directly, exactly as Phase 1 proved ptrace informally; the interpreted
// runtimes cannot even reach these calls (e.g. py3 has no ctypes — libffi is not
// in its mount profile — and no mount binary is bound for bash). Both halves of
// the response matter to these tests: the build (gcc) must succeed so we know the
// program was actually compiled and run, and the run result carries the outcome
// of the attempt. A seccomp KILL is SIGSYS, which nsjail surfaces as a non-zero
// exit, so the API reports the run as runtime_error (see internal/api/handlers.go).
func runC(t *testing.T, source, expectedStdout string) runResponse {
	t.Helper()
	resp := submit(t, runRequest{
		Language:         "c",
		Source:           source,
		SourceFilename:   "solution.c",
		ArtifactFilename: "solution",
		Tests:            []testCase{{Stdin: "", ExpectedStdout: expectedStdout}},
	})
	if resp.Build == nil {
		t.Fatalf("c run returned no build result (top status %q)", resp.Status)
	}
	if len(resp.Tests) != 1 {
		t.Fatalf("expected exactly 1 test result, got %d (top status %q)", len(resp.Tests), resp.Status)
	}
	return resp
}

// assertSeccompKilled asserts the standard shape of a syscall blocked by the
// seccomp deny-list: the C program compiled (build ok) and ran far enough to
// print its "BEFORE" marker, then was killed the instant it issued the denied
// syscall — so the run is runtime_error and the post-syscall "AFTER" marker never
// printed. Seeing "AFTER" would mean the syscall returned instead of killing the
// process: a real escape (the deny-list missed it).
func assertSeccompKilled(t *testing.T, resp runResponse, syscallName string) {
	t.Helper()
	build := resp.Build
	run := resp.Tests[0]
	t.Logf("%s: build=%s run=%s stdout=%q stderr=%q", syscallName, build.Status, run.Status, run.Stdout, run.Stderr)

	if build.Status != "ok" {
		t.Fatalf("%s: C build did not succeed (status %q, stderr %q) — cannot conclude anything about the syscall", syscallName, build.Status, build.Stderr)
	}
	if !strings.Contains(run.Stdout, "BEFORE") {
		t.Errorf("%s: program did not reach the BEFORE marker — it failed before the syscall, so the kill is unproven; stdout=%q stderr=%q", syscallName, run.Stdout, run.Stderr)
	}
	if strings.Contains(run.Stdout, "AFTER") {
		t.Errorf("ESCAPE: %s syscall returned instead of killing the process — the seccomp deny-list did not block it; stdout=%q", syscallName, run.Stdout)
	}
	if run.Status != "runtime_error" {
		t.Errorf("%s: expected run status runtime_error (SIGSYS kill), got %q; stdout=%q stderr=%q", syscallName, run.Status, run.Stdout, run.Stderr)
	}
}
