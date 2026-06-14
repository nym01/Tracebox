package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSaveAndGetRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	run := RunRecord{
		RunID:         "01890000-0000-7000-8000-000000000001",
		Language:      "py3",
		Status:        "accepted",
		ExitCode:      0,
		DurationMs:    42,
		MemoryPeakKB:  1234,
		Timestamp:     "2026-06-14T00:00:00Z",
		Source:        "print('hello')",
		Stdout:        "hello\n",
		Stderr:        "",
		CompileOutput: "",
	}
	events := []TraceEventRecord{
		{Event: "file_open", Syscall: "openat", Path: "/etc/passwd", Timestamp: "2026-06-14T00:00:00.1Z"},
		{Event: "exec", Syscall: "execve", Path: "/bin/ls", Argv: []string{"ls", "-la"}, Timestamp: "2026-06-14T00:00:00.2Z"},
		{Event: "connect", Syscall: "connect", DestIP: "8.8.8.8", DestPort: 53, Timestamp: "2026-06-14T00:00:00.3Z"},
	}

	if err := s.SaveRun(ctx, run, events); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	got, gotEvents, err := s.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got != run {
		t.Errorf("run mismatch:\n got %+v\nwant %+v", got, run)
	}
	if len(gotEvents) != 3 {
		t.Fatalf("expected 3 events, got %d", len(gotEvents))
	}
	if gotEvents[1].Event != "exec" || len(gotEvents[1].Argv) != 2 || gotEvents[1].Argv[1] != "-la" {
		t.Errorf("exec argv not round-tripped: %+v", gotEvents[1])
	}
	if gotEvents[2].DestIP != "8.8.8.8" || gotEvents[2].DestPort != 53 {
		t.Errorf("connect dest not round-tripped: %+v", gotEvents[2])
	}
	// file_open should have no connect fields populated.
	if gotEvents[0].DestIP != "" || gotEvents[0].DestPort != 0 {
		t.Errorf("file_open leaked connect fields: %+v", gotEvents[0])
	}
}

func TestGetRunNotFound(t *testing.T) {
	s := openTestStore(t)
	_, _, err := s.GetRun(context.Background(), "does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestListRuns(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if err := s.SaveRun(ctx, RunRecord{RunID: id, Language: "py3", Status: "accepted", Timestamp: "t"}, nil); err != nil {
			t.Fatalf("SaveRun %s: %v", id, err)
		}
	}
	runs, err := s.ListRuns(ctx, 50)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("expected 3 runs, got %d", len(runs))
	}
	// Newest first.
	if runs[0].RunID != "c" {
		t.Errorf("expected newest run 'c' first, got %q", runs[0].RunID)
	}

	limited, err := s.ListRuns(ctx, 2)
	if err != nil {
		t.Fatalf("ListRuns limited: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("expected 2 runs with limit=2, got %d", len(limited))
	}
}

func TestNilStoreIsSafe(t *testing.T) {
	var s *Store
	if err := s.SaveRun(context.Background(), RunRecord{}, nil); err != nil {
		t.Errorf("nil SaveRun: %v", err)
	}
	if _, _, err := s.GetRun(context.Background(), "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("nil GetRun: expected ErrNotFound, got %v", err)
	}
	if runs, err := s.ListRuns(context.Background(), 10); err != nil || runs != nil {
		t.Errorf("nil ListRuns: got %v, %v", runs, err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}
}
