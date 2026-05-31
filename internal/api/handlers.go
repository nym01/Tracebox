package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/nym01/goboxd/internal/compare"
	"github.com/nym01/goboxd/internal/language"
	"github.com/nym01/goboxd/internal/runner"
	"github.com/nym01/goboxd/internal/status"
)

var defaultRunner runner.Runner = runner.SubprocessRunner{}

func RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("POST /run", run)
}

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
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
	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
		buildArgs := make([]string, len(lang.Build.Args))
		for j, a := range lang.Build.Args {
			buildArgs[j] = resolveTokens(a, srcFilename, artifactFilename)
		}
		buildLimits := effectiveLimits(lang.Build.Limits, req.Build)
		buildArgs = append(buildArgs, requestFlags(req.Build)...)
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
		args := make([]string, len(lang.Run.Args))
		for j, a := range lang.Run.Args {
			args[j] = resolveTokens(a, srcFilename, artifactFilename)
		}
		args = append(args, requestFlags(req.Run)...)

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

// effectiveLimits applies any per-request limits over the language defaults.
// A request value of zero means "not provided" and keeps the default.
func effectiveLimits(base language.Limits, override *PhaseConfig) language.Limits {
	if override == nil || override.Limits == nil {
		return base
	}
	if override.Limits.WallTimeS > 0 {
		base.WallTimeS = override.Limits.WallTimeS
	}
	if override.Limits.MemoryKB > 0 {
		base.MemoryKB = override.Limits.MemoryKB
	}
	if override.Limits.MaxProcesses > 0 {
		base.MaxProcesses = override.Limits.MaxProcesses
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
