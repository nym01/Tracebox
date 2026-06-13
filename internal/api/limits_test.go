package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nym01/goboxd/internal/language"
	"github.com/nym01/goboxd/internal/runner"
)

// ---- effectiveLimits unit tests ----

func TestEffectiveLimitsNoOverride(t *testing.T) {
	base := language.Limits{WallTimeS: 9, MemoryKB: 102400, MaxProcesses: 100}

	for _, override := range []*PhaseConfig{nil, {}} {
		got := effectiveLimits(base, override)
		if got != base {
			t.Errorf("override=%v: got %+v, want %+v", override, got, base)
		}
	}
}

func TestEffectiveLimitsLowerThanDefault(t *testing.T) {
	base := language.Limits{WallTimeS: 9, MemoryKB: 102400, MaxProcesses: 100}
	cases := []struct {
		name  string
		req   language.Limits
		want  language.Limits
	}{
		{
			name: "wall_time lower than default",
			req:  language.Limits{WallTimeS: 3},
			want: language.Limits{WallTimeS: 3, MemoryKB: 102400, MaxProcesses: 100},
		},
		{
			name: "memory lower than default",
			req:  language.Limits{MemoryKB: 51200},
			want: language.Limits{WallTimeS: 9, MemoryKB: 51200, MaxProcesses: 100},
		},
		{
			name: "max_processes lower than default",
			req:  language.Limits{MaxProcesses: 10},
			want: language.Limits{WallTimeS: 9, MemoryKB: 102400, MaxProcesses: 10},
		},
		{
			name: "all fields lower than default",
			req:  language.Limits{WallTimeS: 1, MemoryKB: 1024, MaxProcesses: 1},
			want: language.Limits{WallTimeS: 1, MemoryKB: 1024, MaxProcesses: 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			override := &PhaseConfig{Limits: &tc.req}
			got := effectiveLimits(base, override)
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestEffectiveLimitsHigherThanDefaultCapped(t *testing.T) {
	base := language.Limits{WallTimeS: 9, MemoryKB: 102400, MaxProcesses: 100}
	cases := []struct {
		name string
		req  language.Limits
		want language.Limits
	}{
		{
			name: "wall_time higher — capped at default",
			req:  language.Limits{WallTimeS: 15},
			want: language.Limits{WallTimeS: 9, MemoryKB: 102400, MaxProcesses: 100},
		},
		{
			name: "memory higher — capped at default",
			req:  language.Limits{MemoryKB: 999999},
			want: language.Limits{WallTimeS: 9, MemoryKB: 102400, MaxProcesses: 100},
		},
		{
			name: "max_processes higher — capped at default",
			req:  language.Limits{MaxProcesses: 9999},
			want: language.Limits{WallTimeS: 9, MemoryKB: 102400, MaxProcesses: 100},
		},
		{
			name: "all fields higher — all capped",
			req:  language.Limits{WallTimeS: 100, MemoryKB: 999999, MaxProcesses: 500},
			want: base,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			override := &PhaseConfig{Limits: &tc.req}
			got := effectiveLimits(base, override)
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestEffectiveLimitsMissingFieldsFallBackToDefault(t *testing.T) {
	base := language.Limits{WallTimeS: 9, MemoryKB: 102400, MaxProcesses: 100}

	// Limits struct with only WallTimeS set; MemoryKB and MaxProcesses are zero
	// (meaning "not provided").
	override := &PhaseConfig{Limits: &language.Limits{WallTimeS: 3}}
	got := effectiveLimits(base, override)
	want := language.Limits{WallTimeS: 3, MemoryKB: 102400, MaxProcesses: 100}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestEffectiveLimitsPartialOverride(t *testing.T) {
	base := language.Limits{WallTimeS: 9, MemoryKB: 102400, MaxProcesses: 100}
	cases := []struct {
		name string
		req  language.Limits
		want language.Limits
	}{
		{
			name: "only wall_time overridden",
			req:  language.Limits{WallTimeS: 5},
			want: language.Limits{WallTimeS: 5, MemoryKB: 102400, MaxProcesses: 100},
		},
		{
			name: "only memory overridden",
			req:  language.Limits{MemoryKB: 20480},
			want: language.Limits{WallTimeS: 9, MemoryKB: 20480, MaxProcesses: 100},
		},
		{
			name: "only max_processes overridden",
			req:  language.Limits{MaxProcesses: 5},
			want: language.Limits{WallTimeS: 9, MemoryKB: 102400, MaxProcesses: 5},
		},
		{
			name: "wall_time and memory overridden, max_processes untouched",
			req:  language.Limits{WallTimeS: 4, MemoryKB: 40960},
			want: language.Limits{WallTimeS: 4, MemoryKB: 40960, MaxProcesses: 100},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			override := &PhaseConfig{Limits: &tc.req}
			got := effectiveLimits(base, override)
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestEffectiveLimitsCPUMsPerSec covers the cgroup CPU-bandwidth field
// (cpu_ms_per_sec): like the other limits it falls back to the default when the
// request omits it, is honoured when the request asks for a tighter value, and is
// capped at the default when the request asks for a looser (larger) one — clients
// may only ever tighten resource limits, never loosen them.
func TestEffectiveLimitsCPUMsPerSec(t *testing.T) {
	base := language.Limits{WallTimeS: 9, MemoryKB: 102400, MaxProcesses: 100, CPUMsPerSec: 2000}
	cases := []struct {
		name string
		req  language.Limits
		want language.Limits
	}{
		{
			name: "omitted — keeps default",
			req:  language.Limits{WallTimeS: 3},
			want: language.Limits{WallTimeS: 3, MemoryKB: 102400, MaxProcesses: 100, CPUMsPerSec: 2000},
		},
		{
			name: "lower — honoured",
			req:  language.Limits{CPUMsPerSec: 500},
			want: language.Limits{WallTimeS: 9, MemoryKB: 102400, MaxProcesses: 100, CPUMsPerSec: 500},
		},
		{
			name: "higher — capped at default",
			req:  language.Limits{CPUMsPerSec: 8000},
			want: language.Limits{WallTimeS: 9, MemoryKB: 102400, MaxProcesses: 100, CPUMsPerSec: 2000},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveLimits(base, &PhaseConfig{Limits: &tc.req})
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// ---- handler-level tests that verify limits flow through to the runner ----

// py3 default run wall_time_s is 9 (from configs/languages.yaml).
// These tests use capturingRunner to inspect the WallTimeSec sent to Run().

func TestRunHandlerLimitLowerThanDefault(t *testing.T) {
	orig := defaultRunner
	cap := &capturingRunner{results: []runner.RunResult{{Stdout: "hi\n", ExitCode: 0}}}
	defaultRunner = cap
	defer func() { defaultRunner = orig }()

	body := `{"language":"py3","source":"print('hi')","run":{"limits":{"wall_time_s":3}},"tests":[{"stdin":"","expected_stdout":"hi\n"}]}`
	w := httptest.NewRecorder()
	run(w, httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}
	if len(cap.specs) != 1 {
		t.Fatalf("expected 1 Run call, got %d", len(cap.specs))
	}
	if cap.specs[0].WallTimeSec != 3 {
		t.Errorf("WallTimeSec: want 3, got %d", cap.specs[0].WallTimeSec)
	}
}

func TestRunHandlerLimitHigherThanDefaultCapped(t *testing.T) {
	orig := defaultRunner
	cap := &capturingRunner{results: []runner.RunResult{{Stdout: "hi\n", ExitCode: 0}}}
	defaultRunner = cap
	defer func() { defaultRunner = orig }()

	// py3 default wall_time_s is 9; request sends 15 — must be capped at 9.
	body := `{"language":"py3","source":"print('hi')","run":{"limits":{"wall_time_s":15}},"tests":[{"stdin":"","expected_stdout":"hi\n"}]}`
	w := httptest.NewRecorder()
	run(w, httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}
	if len(cap.specs) != 1 {
		t.Fatalf("expected 1 Run call, got %d", len(cap.specs))
	}
	if cap.specs[0].WallTimeSec != 9 {
		t.Errorf("WallTimeSec: want 9 (capped), got %d", cap.specs[0].WallTimeSec)
	}
}

func TestRunHandlerMissingLimitUsesDefault(t *testing.T) {
	orig := defaultRunner
	cap := &capturingRunner{results: []runner.RunResult{{Stdout: "hi\n", ExitCode: 0}}}
	defaultRunner = cap
	defer func() { defaultRunner = orig }()

	// No limits in request; py3 default wall_time_s is 9.
	body := `{"language":"py3","source":"print('hi')","tests":[{"stdin":"","expected_stdout":"hi\n"}]}`
	w := httptest.NewRecorder()
	run(w, httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}
	if len(cap.specs) != 1 {
		t.Fatalf("expected 1 Run call, got %d", len(cap.specs))
	}
	if cap.specs[0].WallTimeSec != 9 {
		t.Errorf("WallTimeSec: want 9 (language default), got %d", cap.specs[0].WallTimeSec)
	}
}

func TestBodyOverMaxSize(t *testing.T) {
	// Body is the JSON wrapper (~60 bytes) plus 1 MiB of source — exceeds 1 MiB cap.
	bigSrc := strings.Repeat("a", 1<<20)
	body := `{"language":"py3","source":"` + bigSrc + `","tests":[{"stdin":"","expected_stdout":""}]}`
	req := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(body))
	w := httptest.NewRecorder()
	run(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	var errResp errorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp.Error.Code != "request_too_large" {
		t.Errorf("want code request_too_large, got %q", errResp.Error.Code)
	}
}

func TestTestStdinOverMaxSize(t *testing.T) {
	bigStdin := strings.Repeat("x", maxTestFieldBytes+1)
	body := `{"language":"py3","source":"print('hi')","tests":[{"stdin":"` + bigStdin + `","expected_stdout":"hi\n"}]}`
	req := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(body))
	w := httptest.NewRecorder()
	run(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	var errResp errorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp.Error.Code != "invalid_tests" {
		t.Errorf("want code invalid_tests, got %q", errResp.Error.Code)
	}
}

func TestTestExpectedStdoutOverMaxSize(t *testing.T) {
	bigExpected := strings.Repeat("y", maxTestFieldBytes+1)
	body := `{"language":"py3","source":"print('hi')","tests":[{"stdin":"","expected_stdout":"` + bigExpected + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(body))
	w := httptest.NewRecorder()
	run(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	var errResp errorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp.Error.Code != "invalid_tests" {
		t.Errorf("want code invalid_tests, got %q", errResp.Error.Code)
	}
}

func TestRunHandlerPartialLimitOverride(t *testing.T) {
	orig := defaultRunner
	// cpp has build + run phases. Give build a success result, run a success result.
	cap := &capturingRunner{results: []runner.RunResult{
		{ExitCode: 0},
		{Stdout: "hi\n", ExitCode: 0},
	}}
	defaultRunner = cap
	defer func() { defaultRunner = orig }()

	// Override only run.limits.wall_time_s; leave memory and max_processes unset.
	// cpp default run wall_time_s is 3; we send 1. Build limits are untouched.
	body := `{"language":"cpp","source":"#include<iostream>\nint main(){std::cout<<\"hi\\n\";}","source_filename":"solution.cpp","artifact_filename":"solution","run":{"limits":{"wall_time_s":1}},"tests":[{"stdin":"","expected_stdout":"hi\n"}]}`
	w := httptest.NewRecorder()
	run(w, httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", w.Code)
	}
	if len(cap.specs) != 2 {
		t.Fatalf("expected 2 Run calls (build+run), got %d", len(cap.specs))
	}
	// Build wall_time_s should be 3 (cpp default), not 1.
	if cap.specs[0].WallTimeSec != 3 {
		t.Errorf("build WallTimeSec: want 3 (default), got %d", cap.specs[0].WallTimeSec)
	}
	// Run wall_time_s should be 1 (request override).
	if cap.specs[1].WallTimeSec != 1 {
		t.Errorf("run WallTimeSec: want 1 (override), got %d", cap.specs[1].WallTimeSec)
	}

	var resp RunResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "accepted" {
		t.Errorf("top-level status: want accepted, got %q", resp.Status)
	}
}
