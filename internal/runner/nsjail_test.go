package runner

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

// Compile-time assertions that both runner implementations satisfy the
// Runner interface. If either type drifts from the interface, the package
// fails to compile.
var (
	_ Runner = NsjailRunner{}
	_ Runner = SubprocessRunner{}
)

// withStubbedPy3Resolver swaps resolvePy3Mounts for the duration of a test and
// restores it afterward.
func withStubbedPy3Resolver(t *testing.T, fn func(context.Context) ([]string, error)) {
	t.Helper()
	orig := resolvePy3Mounts
	t.Cleanup(func() { resolvePy3Mounts = orig })
	resolvePy3Mounts = fn
}

// TestPy3MountsResolvedOnceAndReused verifies that the expensive py3 mount
// resolution (which shells out to ldd) happens exactly once, at construction,
// and that every subsequent request reuses the cached result rather than
// re-resolving.
func TestPy3MountsResolvedOnceAndReused(t *testing.T) {
	cached := []string{
		"--bindmount_ro", "/usr/bin/python3.11:/usr/bin/python3",
		"--bindmount_ro", "/lib/x86_64-linux-gnu/libc.so.6",
		"--bindmount_ro", "/usr/lib/python3.11",
	}

	var calls int
	withStubbedPy3Resolver(t, func(ctx context.Context) ([]string, error) {
		calls++
		// Return a fresh copy so any accidental mutation by the caller cannot
		// hide a re-resolution behind shared state.
		return append([]string(nil), cached...), nil
	})

	r, err := NewNsjailRunner(context.Background(), "nsjail")
	if err != nil {
		t.Fatalf("NewNsjailRunner: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected resolver to run once at construction, ran %d times", calls)
	}

	// Many py3 requests, each with a different work directory. None of them
	// should trigger another resolution, and each should carry the cached
	// read-only mounts plus its own writable work dir.
	for i := range 5 {
		workDir := fmt.Sprintf("/tmp/goboxd-%d", i)
		got := r.filesystemArgs(RunSpec{Cmd: pythonInterpreter, WorkDir: workDir})

		want := append(append([]string(nil), cached...), "--bindmount", workDir)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("request %d: filesystemArgs = %v, want %v", i, got, want)
		}
	}

	if calls != 1 {
		t.Fatalf("resolver re-ran on subsequent requests: ran %d times total", calls)
	}
}

// TestFilesystemArgsDoesNotMutateCache guards against the per-request work-dir
// append corrupting the shared cached slice across requests.
func TestFilesystemArgsDoesNotMutateCache(t *testing.T) {
	cached := []string{"--bindmount_ro", "/usr/bin/python3.11:/usr/bin/python3"}
	withStubbedPy3Resolver(t, func(ctx context.Context) ([]string, error) {
		return append([]string(nil), cached...), nil
	})

	r, err := NewNsjailRunner(context.Background(), "nsjail")
	if err != nil {
		t.Fatalf("NewNsjailRunner: %v", err)
	}

	r.filesystemArgs(RunSpec{Cmd: pythonInterpreter, WorkDir: "/tmp/a"})
	r.filesystemArgs(RunSpec{Cmd: pythonInterpreter, WorkDir: "/tmp/b"})

	if !reflect.DeepEqual(r.py3Mounts, cached) {
		t.Fatalf("cached py3 mounts were mutated: got %v, want %v", r.py3Mounts, cached)
	}
}

// TestNewNsjailRunnerFailsLoudlyOnResolveError confirms a resolution failure at
// startup surfaces as a construction error (so main can make it fatal) rather
// than being deferred to the first request.
func TestNewNsjailRunnerFailsLoudlyOnResolveError(t *testing.T) {
	withStubbedPy3Resolver(t, func(ctx context.Context) ([]string, error) {
		return nil, fmt.Errorf("python3 not found")
	})

	if _, err := NewNsjailRunner(context.Background(), "nsjail"); err == nil {
		t.Fatal("expected NewNsjailRunner to return an error when py3 mounts cannot be resolved")
	}
}

// TestFilesystemArgsNonPy3Unchanged confirms non-py3 languages still get the
// shared-host-filesystem flag and are unaffected by the py3 cache.
func TestFilesystemArgsNonPy3Unchanged(t *testing.T) {
	withStubbedPy3Resolver(t, func(ctx context.Context) ([]string, error) {
		return []string{"--bindmount_ro", "/usr/bin/python3.11:/usr/bin/python3"}, nil
	})

	r, err := NewNsjailRunner(context.Background(), "nsjail")
	if err != nil {
		t.Fatalf("NewNsjailRunner: %v", err)
	}

	got := r.filesystemArgs(RunSpec{Cmd: "/usr/bin/g++", WorkDir: "/tmp/x"})
	want := []string{"--disable_clone_newns"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("non-py3 filesystemArgs = %v, want %v", got, want)
	}
}
