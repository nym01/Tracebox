package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nym01/goboxd/internal/store"
	"github.com/nym01/goboxd/internal/tracer"
)

// runStore persists completed runs and their trace events, or nil when
// persistence is disabled (Open failed at startup, or it was never wired). Its
// methods are nil-safe, so the run handler can call persistRun unconditionally —
// the same pattern as fileTracer.
var runStore *store.Store

// SetStore installs the SQLite-backed run store. Called once at startup; nil
// leaves persistence disabled (stdout logging continues regardless).
func SetStore(s *store.Store) {
	runStore = s
}

// persistRun writes a completed run and its trace events to the store, alongside
// (not replacing) the stdout logging done by emitRunLog/emitTraceEvents. It is
// best-effort: a write failure is logged but never fails the run, since the
// authoritative response has already been computed and the stdout log line still
// records the completion. No-op when persistence is disabled.
//
// The single runs row aggregates the per-test output: stdout/stderr are the test
// outputs joined (one entry per test, which is exactly that test's output in the
// common single-test case), memory_peak_kb is the max across tests, and
// compile_output is the build phase's combined output (empty for interpreted
// languages). source is the exact code submitted, so the row is a self-contained
// "what code produced what result" audit record.
func persistRun(ctx context.Context, runID, language, status string, exitCode int, durationMs int64, timestamp, source string, build *BuildResult, tests []TestResult, traceRun *tracer.Run) {
	if runStore == nil {
		return
	}

	var stdoutParts, stderrParts []string
	var memPeak int64
	for _, t := range tests {
		stdoutParts = append(stdoutParts, t.Stdout)
		stderrParts = append(stderrParts, t.Stderr)
		if t.MemoryPeakKB > memPeak {
			memPeak = t.MemoryPeakKB
		}
	}

	var compileOutput string
	if build != nil {
		compileOutput = joinNonEmpty(build.Stdout, build.Stderr)
	}

	rec := store.RunRecord{
		RunID:         runID,
		Language:      language,
		Status:        status,
		ExitCode:      exitCode,
		DurationMs:    durationMs,
		MemoryPeakKB:  memPeak,
		Timestamp:     timestamp,
		Source:        source,
		Stdout:        strings.Join(stdoutParts, ""),
		Stderr:        strings.Join(stderrParts, ""),
		CompileOutput: compileOutput,
	}

	events := traceEventRecords(traceRun)
	if err := runStore.SaveRun(ctx, rec, events); err != nil {
		log.Printf("store: failed to persist run %s: %v", runID, err)
	}
}

// traceEventRecords converts the tracer's captured events into store records. It
// mirrors emitTraceEvents but builds rows for persistence instead of log lines.
func traceEventRecords(run *tracer.Run) []store.TraceEventRecord {
	evs := run.Events()
	if len(evs) == 0 {
		return nil
	}
	out := make([]store.TraceEventRecord, 0, len(evs))
	for _, ev := range evs {
		rec := store.TraceEventRecord{
			Event:     ev.Kind,
			Syscall:   ev.Syscall,
			Timestamp: ev.Time.Format(time.RFC3339Nano),
		}
		switch ev.Kind {
		case "exec":
			rec.Path = ev.Path
			rec.Argv = ev.Argv
		case "connect":
			rec.DestIP = ev.DestIP
			rec.DestPort = ev.DestPort
		default: // "file_open"
			rec.Path = ev.Path
		}
		out = append(out, rec)
	}
	return out
}

// runDetailResponse is the GET /runs/{run_id} body: the run's metadata plus all
// associated trace events.
type runDetailResponse struct {
	store.RunRecord
	TraceEvents []store.TraceEventRecord `json:"trace_events"`
}

// runsListResponse is the GET /runs body: a page of recent run metadata.
type runsListResponse struct {
	Runs []store.RunRecord `json:"runs"`
}

// getRunHandler serves GET /runs/{run_id}: the full audit record for one run
// (metadata, source, captured output, and trace events), or 404 if unknown.
func getRunHandler(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	rec, events, err := runStore.GetRun(r.Context(), runID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "run_not_found", "no run with that id")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to load run")
		return
	}
	if events == nil {
		events = []store.TraceEventRecord{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(runDetailResponse{RunRecord: rec, TraceEvents: events})
}

// listRunsHandler serves GET /runs: recent runs for browsing without a specific
// id. Accepts an optional ?limit= (1..200, default 50).
func listRunsHandler(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = min(n, 200)
		}
	}
	runs, err := runStore.ListRuns(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list runs")
		return
	}
	if runs == nil {
		runs = []store.RunRecord{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(runsListResponse{Runs: runs})
}

// joinNonEmpty joins the non-empty parts with a newline, so an empty stdout or
// stderr does not introduce a leading/trailing blank line in compile_output.
func joinNonEmpty(parts ...string) string {
	kept := parts[:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n")
}
