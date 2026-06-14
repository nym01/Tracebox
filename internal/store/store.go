// Package store persists completed /run results and their eBPF trace events to a
// SQLite database, turning the existing structured stdout logs into a queryable
// audit trail. It is purely additive: the stdout logging in internal/api stays
// as-is for live tailing, and persistence failures never fail a run (the caller
// logs and continues, mirroring how the eBPF tracer degrades).
//
// SQLite driver choice: this uses modernc.org/sqlite, a pure-Go (cgo-free)
// implementation, because the binary is built with CGO_ENABLED=0 (see the
// Dockerfile). The cgo-based mattn/go-sqlite3 would require flipping CGO on for
// the whole build, which the eBPF embedding (bpf2go) is specifically arranged to
// avoid. modernc.org/sqlite registers itself under the driver name "sqlite".
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned by GetRun when no run with the given id is stored.
var ErrNotFound = errors.New("run not found")

// Store is the persistence handle. Its methods are nil-safe: a nil *Store means
// persistence is disabled (Open failed at startup), so SaveRun is a no-op and the
// read methods report no data. This lets the caller wire the store unconditionally,
// the same way the eBPF tracer is wired.
type Store struct {
	db *sql.DB
}

// RunRecord is one persisted run: the completion metadata that also appears in the
// structured stdout log, plus the audit-trail fields (source, captured output)
// that the log line omits.
type RunRecord struct {
	RunID         string `json:"run_id"`
	Language      string `json:"language"`
	Status        string `json:"status"`
	ExitCode      int    `json:"exit_code"`
	DurationMs    int64  `json:"duration_ms"`
	MemoryPeakKB  int64  `json:"memory_peak_kb"`
	Timestamp     string `json:"timestamp"`
	Source        string `json:"source"`
	Stdout        string `json:"stdout"`
	Stderr        string `json:"stderr"`
	CompileOutput string `json:"compile_output"`
}

// TraceEventRecord is one persisted eBPF trace event correlated to a run. The
// type-specific fields are populated per Event: Path/Argv for file_open and exec,
// DestIP/DestPort for connect. Empty/zero values are normal for the inapplicable
// fields of a given event type.
type TraceEventRecord struct {
	Event     string   `json:"event"`
	Syscall   string   `json:"syscall"`
	Path      string   `json:"path,omitempty"`
	Argv      []string `json:"argv,omitempty"`
	DestIP    string   `json:"dest_ip,omitempty"`
	DestPort  int      `json:"dest_port,omitempty"`
	Timestamp string   `json:"timestamp"`
}

const schema = `
CREATE TABLE IF NOT EXISTS runs (
	run_id         TEXT PRIMARY KEY,
	language       TEXT NOT NULL,
	status         TEXT NOT NULL,
	exit_code      INTEGER NOT NULL,
	duration_ms    INTEGER NOT NULL,
	memory_peak_kb INTEGER NOT NULL,
	timestamp      TEXT NOT NULL,
	source         TEXT NOT NULL,
	stdout         TEXT NOT NULL,
	stderr         TEXT NOT NULL,
	compile_output TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS trace_events (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id    TEXT NOT NULL REFERENCES runs(run_id),
	event     TEXT NOT NULL,
	syscall   TEXT NOT NULL,
	path      TEXT,
	argv      TEXT,
	dest_ip   TEXT,
	dest_port INTEGER,
	timestamp TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_trace_events_run_id ON trace_events(run_id);
`

// Open opens (creating if needed) the SQLite database at path, creates the parent
// directory and applies the schema. It returns a usable *Store, or an error the
// caller should log before continuing with persistence disabled (a nil *Store).
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: create db dir %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// One writer at a time keeps concurrent /run requests from tripping SQLITE_BUSY;
	// writes are tiny and off the response's critical path, so this is cheap.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database. Safe on a nil *Store.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// SaveRun persists a completed run and its trace events in a single transaction.
// Safe on a nil *Store (no-op, returns nil) so the caller can invoke it
// unconditionally when persistence is disabled.
func (s *Store) SaveRun(ctx context.Context, run RunRecord, events []TraceEventRecord) error {
	if s == nil || s.db == nil {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO runs (run_id, language, status, exit_code, duration_ms,
			memory_peak_kb, timestamp, source, stdout, stderr, compile_output)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.RunID, run.Language, run.Status, run.ExitCode, run.DurationMs,
		run.MemoryPeakKB, run.Timestamp, run.Source, run.Stdout, run.Stderr, run.CompileOutput,
	); err != nil {
		return fmt.Errorf("store: insert run: %w", err)
	}

	for _, ev := range events {
		var argv any
		if len(ev.Argv) > 0 {
			b, err := json.Marshal(ev.Argv)
			if err == nil {
				argv = string(b)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO trace_events (run_id, event, syscall, path, argv, dest_ip, dest_port, timestamp)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			run.RunID, ev.Event, ev.Syscall,
			nullIfEmpty(ev.Path), argv, nullIfEmpty(ev.DestIP), ev.DestPort, ev.Timestamp,
		); err != nil {
			return fmt.Errorf("store: insert trace event: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// GetRun returns the run with the given id and its trace events (ordered by
// insertion). It returns ErrNotFound if no such run exists, or if persistence is
// disabled (nil *Store).
func (s *Store) GetRun(ctx context.Context, runID string) (RunRecord, []TraceEventRecord, error) {
	if s == nil || s.db == nil {
		return RunRecord{}, nil, ErrNotFound
	}
	var run RunRecord
	err := s.db.QueryRowContext(ctx,
		`SELECT run_id, language, status, exit_code, duration_ms, memory_peak_kb,
			timestamp, source, stdout, stderr, compile_output
		 FROM runs WHERE run_id = ?`, runID,
	).Scan(
		&run.RunID, &run.Language, &run.Status, &run.ExitCode, &run.DurationMs,
		&run.MemoryPeakKB, &run.Timestamp, &run.Source, &run.Stdout, &run.Stderr, &run.CompileOutput,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return RunRecord{}, nil, ErrNotFound
	}
	if err != nil {
		return RunRecord{}, nil, fmt.Errorf("store: query run: %w", err)
	}

	events, err := s.traceEvents(ctx, runID)
	if err != nil {
		return RunRecord{}, nil, err
	}
	return run, events, nil
}

// traceEvents loads the trace events for one run, ordered by insertion id.
func (s *Store) traceEvents(ctx context.Context, runID string) ([]TraceEventRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT event, syscall, path, argv, dest_ip, dest_port, timestamp
		 FROM trace_events WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: query trace events: %w", err)
	}
	defer rows.Close()

	var events []TraceEventRecord
	for rows.Next() {
		var (
			ev      TraceEventRecord
			path    sql.NullString
			argv    sql.NullString
			destIP  sql.NullString
			destPrt sql.NullInt64
		)
		if err := rows.Scan(&ev.Event, &ev.Syscall, &path, &argv, &destIP, &destPrt, &ev.Timestamp); err != nil {
			return nil, fmt.Errorf("store: scan trace event: %w", err)
		}
		ev.Path = path.String
		ev.DestIP = destIP.String
		ev.DestPort = int(destPrt.Int64)
		if argv.Valid && argv.String != "" {
			_ = json.Unmarshal([]byte(argv.String), &ev.Argv)
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}

// ListRuns returns up to limit most-recent runs (newest first by insertion),
// without their trace events — a lightweight index for browsing. Returns nil
// when persistence is disabled.
func (s *Store) ListRuns(ctx context.Context, limit int) ([]RunRecord, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT run_id, language, status, exit_code, duration_ms, memory_peak_kb,
			timestamp, source, stdout, stderr, compile_output
		 FROM runs ORDER BY rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}
	defer rows.Close()

	var runs []RunRecord
	for rows.Next() {
		var run RunRecord
		if err := rows.Scan(
			&run.RunID, &run.Language, &run.Status, &run.ExitCode, &run.DurationMs,
			&run.MemoryPeakKB, &run.Timestamp, &run.Source, &run.Stdout, &run.Stderr, &run.CompileOutput,
		); err != nil {
			return nil, fmt.Errorf("store: scan run: %w", err)
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// nullIfEmpty returns a SQL NULL for an empty string so inapplicable
// type-specific columns (e.g. dest_ip on a file_open) store as NULL rather than "".
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
