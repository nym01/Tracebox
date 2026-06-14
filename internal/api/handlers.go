package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nym01/goboxd/internal/compare"
	"github.com/nym01/goboxd/internal/language"
	"github.com/nym01/goboxd/internal/runner"
	"github.com/nym01/goboxd/internal/status"
	"github.com/nym01/goboxd/internal/tracer"
)

const maxBodyBytes = 1 << 20 // 1 MiB

var defaultRunner runner.Runner = runner.SubprocessRunner{}

// fileTracer is the process-wide eBPF file-open tracer, or nil when tracing is
// disabled (non-nsjail runner, non-Linux, or Start failed). Its methods are
// nil-safe, so the run handler uses it unconditionally.
var fileTracer *tracer.Tracer

// SetRunner replaces the Runner used to execute build and test commands.
// Called once at startup to select between SubprocessRunner and NsjailRunner.
func SetRunner(r runner.Runner) {
	defaultRunner = r
}

// SetTracer installs the eBPF file-open tracer used to capture per-run file
// opens. Called once at startup; nil leaves tracing disabled.
func SetTracer(t *tracer.Tracer) {
	fileTracer = t
}

func RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /readyz", readyzHandler)
	mux.HandleFunc("GET /info", infoHandler)
	mux.HandleFunc("POST /run", run)
	mux.HandleFunc("GET /runs", listRunsHandler)
	mux.HandleFunc("GET /runs/{run_id}", getRunHandler)
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
	RunID  string       `json:"run_id"`
	Status string       `json:"status"`
	Build  *BuildResult `json:"build,omitempty"`
	Tests  []TestResult `json:"tests"`
}

// runLog is the structured JSON log line emitted to stdout when a run completes.
type runLog struct {
	RunID      string `json:"run_id"`
	Language   string `json:"language"`
	Status     string `json:"status"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
	Timestamp  string `json:"timestamp"`
}

// emitRunLog writes a single structured JSON log line describing a completed run.
func emitRunLog(l runLog) {
	line, err := json.Marshal(l)
	if err != nil {
		return
	}
	fmt.Fprintln(os.Stdout, string(line))
}

// fileOpenLog is the structured JSON log line emitted for one file the
// sandboxed run opened, captured by the eBPF tracer. run_id correlates it with
// the run's emitRunLog line. One line is emitted per opened file.
type fileOpenLog struct {
	RunID     string `json:"run_id"`
	Event     string `json:"event"` // always "file_open"
	Syscall   string `json:"syscall"`
	Path      string `json:"path"`
	Timestamp string `json:"timestamp"`
}

// execLog is the structured JSON log line emitted for one process the sandboxed
// run spawned (execve/execveat), captured by the eBPF tracer. run_id correlates
// it with the run's emitRunLog line. argv holds the captured argument prefix
// (omitted when nothing was captured). One line is emitted per spawned process.
type execLog struct {
	RunID     string   `json:"run_id"`
	Event     string   `json:"event"` // always "exec"
	Syscall   string   `json:"syscall"`
	Path      string   `json:"path"`
	Argv      []string `json:"argv,omitempty"`
	Timestamp string   `json:"timestamp"`
}

// connectLog is the structured JSON log line emitted for one network connection
// the sandboxed run attempted (connect), captured by the eBPF tracer. run_id
// correlates it with the run's emitRunLog line. The attempt is recorded even
// though the sandbox's empty network namespace makes it fail (ENETUNREACH) — it
// captures intent (the destination the code wanted to reach). One line is emitted
// per connect attempt.
type connectLog struct {
	RunID     string `json:"run_id"`
	Event     string `json:"event"` // always "connect"
	Syscall   string `json:"syscall"`
	DestIP    string `json:"dest_ip"`
	DestPort  int    `json:"dest_port"`
	Timestamp string `json:"timestamp"`
}

// emitTraceEvents writes one structured JSON log line per syscall the run made
// (a file_open, an exec, or a connect), alongside the run-completion log line.
// No-op when tracing is disabled (run is nil) or nothing was captured.
func emitTraceEvents(runID string, run *tracer.Run) {
	for _, ev := range run.Events() {
		var line []byte
		var err error
		switch ev.Kind {
		case "exec":
			line, err = json.Marshal(execLog{
				RunID:     runID,
				Event:     "exec",
				Syscall:   ev.Syscall,
				Path:      ev.Path,
				Argv:      ev.Argv,
				Timestamp: ev.Time.Format(time.RFC3339Nano),
			})
		case "connect":
			line, err = json.Marshal(connectLog{
				RunID:     runID,
				Event:     "connect",
				Syscall:   ev.Syscall,
				DestIP:    ev.DestIP,
				DestPort:  ev.DestPort,
				Timestamp: ev.Time.Format(time.RFC3339Nano),
			})
		default: // "file_open"
			line, err = json.Marshal(fileOpenLog{
				RunID:     runID,
				Event:     "file_open",
				Syscall:   ev.Syscall,
				Path:      ev.Path,
				Timestamp: ev.Time.Format(time.RFC3339Nano),
			})
		}
		if err != nil {
			continue
		}
		fmt.Fprintln(os.Stdout, string(line))
	}
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

	// run_id identifies this run in the response and structured log. Generated
	// before any runner invocation; an RNG failure falls back to the nil UUID
	// rather than aborting an otherwise valid run.
	runID := "00000000-0000-0000-0000-000000000000"
	if v, err := uuid.NewV7(); err == nil {
		runID = v.String()
	}
	runStart := time.Now()

	// Begin collecting the file opens this run makes. Nil-safe when tracing is
	// disabled. Closed at the end of the request to free the kernel-side filter.
	traceRun := fileTracer.NewRun()
	defer traceRun.Close()

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
			Cmd:          buildCmd,
			Args:         buildArgs,
			WorkDir:      tmpDir,
			WallTimeSec:  wallSec,
			MemoryKB:     buildLimits.MemoryKB,
			MaxProcesses: buildLimits.MaxProcesses,
			CPUMsPerSec:  buildLimits.CPUMsPerSec,
			OnStart:      traceRun.Attach,
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
			topStatus := status.TopLevel(bstatus, nil)
			durationMs := time.Since(runStart).Milliseconds()
			timestamp := time.Now().Format(time.RFC3339)
			emitTraceEvents(runID, traceRun)
			emitRunLog(runLog{
				RunID:      runID,
				Language:   req.Language,
				Status:     topStatus,
				ExitCode:   bres.ExitCode,
				DurationMs: durationMs,
				Timestamp:  timestamp,
			})
			persistRun(r.Context(), runID, req.Language, topStatus, bres.ExitCode, durationMs, timestamp, req.Source, buildResult, notExecuted, traceRun)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(RunResponse{
				RunID:  runID,
				Status: topStatus,
				Build:  buildResult,
				Tests:  notExecuted,
			})
			return
		}
	}

	testResults := make([]TestResult, len(req.Tests))

	// runExitCode summarizes the run for the structured log: the first non-zero
	// test exit code, or 0 when every test exited cleanly.
	runExitCode := 0

	for i, tc := range req.Tests {
		cmd := resolveTokens(lang.Run.Cmd, srcFilename, artifactFilename)
		args := expandArgs(lang.Run.Args, srcFilename, artifactFilename, requestFlags(req.Run))

		runLimits := effectiveLimits(lang.Run.Limits, req.Run)
		wallSec := runLimits.WallTimeS
		if wallSec <= 0 {
			wallSec = 10
		}

		result, runErr := rr.Run(r.Context(), runner.RunSpec{
			Cmd:          cmd,
			Args:         args,
			Stdin:        tc.Stdin,
			WorkDir:      tmpDir,
			WallTimeSec:  wallSec,
			MemoryKB:     runLimits.MemoryKB,
			MaxProcesses: runLimits.MaxProcesses,
			CPUMsPerSec:  runLimits.CPUMsPerSec,
			OnStart:      traceRun.Attach,
		})
		if runErr != nil {
			testResults[i] = TestResult{Status: status.InternalError}
			continue
		}

		if result.ExitCode != 0 && runExitCode == 0 {
			runExitCode = result.ExitCode
		}

		var ts string
		switch {
		case result.TimedOut:
			ts = status.TimeExceeded
		case result.MemoryExceeded:
			// The cgroup OOM killer fired. Checked before the generic non-zero
			// exit below, because an OOM kill also surfaces as a non-zero
			// (SIGKILL) exit code but is a distinct, more specific outcome.
			ts = status.MemoryExceeded
		case result.ExitCode != 0:
			ts = status.RuntimeError
		default:
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

	durationMs := time.Since(runStart).Milliseconds()
	timestamp := time.Now().Format(time.RFC3339)
	emitTraceEvents(runID, traceRun)
	emitRunLog(runLog{
		RunID:      runID,
		Language:   req.Language,
		Status:     topStatus,
		ExitCode:   runExitCode,
		DurationMs: durationMs,
		Timestamp:  timestamp,
	})
	persistRun(r.Context(), runID, req.Language, topStatus, runExitCode, durationMs, timestamp, req.Source, buildResult, testResults, traceRun)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(RunResponse{RunID: runID, Status: topStatus, Build: buildResult, Tests: testResults})
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
	if v := override.Limits.CPUMsPerSec; v > 0 {
		if base.CPUMsPerSec > 0 && v > base.CPUMsPerSec {
			v = base.CPUMsPerSec
		}
		base.CPUMsPerSec = v
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
