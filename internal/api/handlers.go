package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/nym01/goboxd/internal/compare"
	"github.com/nym01/goboxd/internal/language"
	"github.com/nym01/goboxd/internal/runner"
	"github.com/nym01/goboxd/internal/status"
)

const maxBodyBytes = 1 << 20 // 1 MiB

var defaultRunner runner.Runner = runner.SubprocessRunner{}

func RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /readyz", readyzHandler)
	mux.HandleFunc("GET /info", infoHandler)
	mux.HandleFunc("POST /run", run)
}

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func readyzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if cachedReadyz == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "not_ready"})
		return
	}
	code := http.StatusOK
	if cachedReadyz.Status != "ok" {
		code = http.StatusServiceUnavailable
	}
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(cachedReadyz)
}

type BuildResult struct {
	Status     string `json:"status"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMs int64  `json:"duration_ms"`
}

type TestResult struct {
	Status       string `json:"status"`
	Stdout       string `json:"stdout"`
	Stderr       string `json:"stderr"`
	DurationMs   int64  `json:"duration_ms"`
	MemoryPeakKB int64  `json:"memory_peak_kb"`
}

type RunResponse struct {
	Status string       `json:"status"`
	Build  *BuildResult `json:"build,omitempty"`
	Tests  []TestResult `json:"tests"`
}

func run(w http.ResponseWriter, r *http.Request) {
	incrementJobsTotal()

	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-r.Context().Done():
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusBadRequest, "request_too_large", "request body exceeds 1 MiB")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		return
	}
	if verr := validateRunRequest(&req); verr != nil {
		writeError(w, http.StatusBadRequest, verr.Code, verr.Message)
		return
	}

	lang, _ := language.Lookup(req.Language)

	tmpDir, err := os.MkdirTemp("", "goboxd-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create working directory")
		return
	}
	defer os.RemoveAll(tmpDir)

	srcFilename := req.SourceFilename
	if srcFilename == "" {
		srcFilename = lang.SourceFilename
	}
	if srcFilename == "" {
		srcFilename = "solution"
	}

	artifactFilename := req.ArtifactFilename
	if artifactFilename == "" {
		artifactFilename = lang.Artifact
	}
	if artifactFilename == "" {
		artifactFilename = "solution"
	}

	if err := os.WriteFile(filepath.Join(tmpDir, srcFilename), []byte(req.Source), 0600); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to write source file")
		return
	}

	rr := defaultRunner

	// Build phase — compiled languages only.
	var buildResult *BuildResult
	if lang.Build != nil {
		buildCmd := resolveTokens(lang.Build.Cmd, srcFilename, artifactFilename)
		buildArgs := expandArgs(lang.Build.Args, srcFilename, artifactFilename, requestFlags(req.Build))
		buildLimits := effectiveLimits(lang.Build.Limits, req.Build)
		wallSec := buildLimits.WallTimeS
		if wallSec <= 0 {
			wallSec = 30
		}

		bres, buildErr := rr.Run(r.Context(), runner.RunSpec{
			Cmd:         buildCmd,
			Args:        buildArgs,
			WorkDir:     tmpDir,
			WallTimeSec: wallSec,
		})
		if buildErr != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "compiler process failed to start")
			return
		}

		bstatus := status.BuildOK
		if bres.TimedOut || bres.ExitCode != 0 {
			bstatus = status.BuildFailed
		}
		buildResult = &BuildResult{
			Status:     bstatus,
			Stdout:     bres.Stdout,
			Stderr:     bres.Stderr,
			DurationMs: bres.DurationMs,
		}

		if bstatus == status.BuildFailed {
			notExecuted := make([]TestResult, len(req.Tests))
			for i := range notExecuted {
				notExecuted[i] = TestResult{Status: status.NotExecuted}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(RunResponse{
				Status: status.TopLevel(bstatus, nil),
				Build:  buildResult,
				Tests:  notExecuted,
			})
			return
		}
	}

	testResults := make([]TestResult, len(req.Tests))

	for i, tc := range req.Tests {
		cmd := resolveTokens(lang.Run.Cmd, srcFilename, artifactFilename)
		args := expandArgs(lang.Run.Args, srcFilename, artifactFilename, requestFlags(req.Run))

		runLimits := effectiveLimits(lang.Run.Limits, req.Run)
		wallSec := runLimits.WallTimeS
		if wallSec <= 0 {
			wallSec = 10
		}

		result, runErr := rr.Run(r.Context(), runner.RunSpec{
			Cmd:         cmd,
			Args:        args,
			Stdin:       tc.Stdin,
			WorkDir:     tmpDir,
			WallTimeSec: wallSec,
		})
		if runErr != nil {
			testResults[i] = TestResult{Status: status.InternalError}
			continue
		}

		var ts string
		if result.TimedOut {
			ts = status.TimeExceeded
		} else if result.ExitCode != 0 {
			ts = status.RuntimeError
		} else {
			ts = compare.Compare(result.Stdout, tc.ExpectedStdout)
		}

		testResults[i] = TestResult{
			Status:       ts,
			Stdout:       result.Stdout,
			Stderr:       result.Stderr,
			DurationMs:   result.DurationMs,
			MemoryPeakKB: result.MemoryPeakKB,
		}
	}

	testStatuses := make([]string, len(testResults))
	for i, tr := range testResults {
		testStatuses[i] = tr.Status
	}
	topStatus := status.TopLevel(status.BuildOK, testStatuses)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(RunResponse{Status: topStatus, Build: buildResult, Tests: testResults})
}

func resolveTokens(s, sourceFile, artifactFile string) string {
	s = strings.ReplaceAll(s, "{{source}}", sourceFile)
	s = strings.ReplaceAll(s, "{{artifact}}", artifactFile)
	return s
}

// expandArgs processes a language's arg template list. The {{flags}} element
// is replaced by the caller-supplied flags slice (expanded inline, or omitted
// if empty). All other elements go through resolveTokens for {{source}} /
// {{artifact}} substitution.
func expandArgs(tmpl []string, sourceFile, artifactFile string, flags []string) []string {
	out := make([]string, 0, len(tmpl)+len(flags))
	for _, a := range tmpl {
		if a == "{{flags}}" {
			out = append(out, flags...)
		} else {
			out = append(out, resolveTokens(a, sourceFile, artifactFile))
		}
	}
	return out
}

// effectiveLimits merges per-request limit overrides onto the language defaults.
// A zero request value means "not provided" and keeps the default.
// A request value higher than the language default is capped at the default —
// clients can only tighten limits, never loosen them.
func effectiveLimits(base language.Limits, override *PhaseConfig) language.Limits {
	if override == nil || override.Limits == nil {
		return base
	}
	if v := override.Limits.WallTimeS; v > 0 {
		if base.WallTimeS > 0 && v > base.WallTimeS {
			v = base.WallTimeS
		}
		base.WallTimeS = v
	}
	if v := override.Limits.MemoryKB; v > 0 {
		if base.MemoryKB > 0 && v > base.MemoryKB {
			v = base.MemoryKB
		}
		base.MemoryKB = v
	}
	if v := override.Limits.MaxProcesses; v > 0 {
		if base.MaxProcesses > 0 && v > base.MaxProcesses {
			v = base.MaxProcesses
		}
		base.MaxProcesses = v
	}
	return base
}

// requestFlags returns the per-request flags for a phase, or nil if none.
func requestFlags(phase *PhaseConfig) []string {
	if phase == nil {
		return nil
	}
	return phase.Flags
}
