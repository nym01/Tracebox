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
)

func RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("POST /run", run)
}

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
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

	rr := runner.SubprocessRunner{}
	testResults := make([]TestResult, len(req.Tests))
	topStatus := "accepted"

	for i, tc := range req.Tests {
		cmd := resolveTokens(lang.Run.Cmd, srcFilename, artifactFilename)
		args := make([]string, len(lang.Run.Args))
		for j, a := range lang.Run.Args {
			args[j] = resolveTokens(a, srcFilename, artifactFilename)
		}

		wallSec := lang.Run.Limits.WallTimeSec
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
			testResults[i] = TestResult{Status: "internal_error"}
			if topStatus == "accepted" {
				topStatus = "internal_error"
			}
			continue
		}

		var status string
		if result.TimedOut {
			status = "time_exceeded"
		} else if result.ExitCode != 0 {
			status = "runtime_error"
		} else {
			status = compare.Compare(result.Stdout, tc.ExpectedStdout)
		}

		if status != "accepted" && topStatus == "accepted" {
			topStatus = status
		}

		testResults[i] = TestResult{
			Status:       status,
			Stdout:       result.Stdout,
			Stderr:       result.Stderr,
			DurationMs:   result.DurationMs,
			MemoryPeakKB: result.MemoryPeakKB,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(RunResponse{Status: topStatus, Tests: testResults})
}

func resolveTokens(s, sourceFile, artifactFile string) string {
	s = strings.ReplaceAll(s, "{{source}}", sourceFile)
	s = strings.ReplaceAll(s, "{{artifact}}", artifactFile)
	return s
}
