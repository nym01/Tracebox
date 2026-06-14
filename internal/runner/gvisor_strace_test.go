package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sample boot-log lines in gVisor's strace format. Each carries the real
// debug-log prefix (D<date> <time> <goroutine> strace.go:NN] [thread]) the parser
// must skip past, then the "<comm> E|X <syscall>(<args>)" strace message.
const (
	lineOpenat   = `D0615 10:20:30.123456      42 strace.go:567] [   1: 1] python3 E openat(AT_FDCWD /work, 0x7f4c0a1b2c00 /usr/lib/python3.11/io.py, O_RDONLY|O_CLOEXEC, 0o0)`
	lineOpenatX  = `D0615 10:20:30.123900      42 strace.go:625] [   1: 1] python3 X openat(AT_FDCWD /work, 0x7f4c0a1b2c00 /usr/lib/python3.11/io.py, O_RDONLY|O_CLOEXEC, 0o0) = 3 (34.278µs)`
	lineExecve   = `D0615 10:20:30.100000      42 strace.go:567] [   1: 1] python3 E execve(0x7f4c0a1b2c00 /usr/bin/python3, 0x7f4c0a1b2d00 ["python3", "solution.py"], 0x7f4c0a1b2e00 ["PATH=/usr/bin", "HOME=/"])`
	lineExecveat = `D0615 10:20:30.110000      42 strace.go:567] [   2: 2] sh E execveat(AT_FDCWD /work, 0x7f4c0a1b3000 /bin/ls, 0x7f4c0a1b3100 ["ls", "-la"], 0x7f4c0a1b3200 ["PATH=/bin"], 0x0)`
	lineConnect4 = `D0615 10:20:30.130000      42 strace.go:567] [   1: 1] python3 E connect(0x3 socket:[12345], 0x7f4c0a1b3300 {Family: AF_INET, Addr: 8.8.8.8, Port: 53}, 0x10)`
	lineConnect6 = `D0615 10:20:30.140000      42 strace.go:567] [   1: 1] python3 E connect(0x3 socket:[12346], 0x7f4c0a1b3400 {Family: AF_INET6, Addr: 2001:4860:4860::8888, Port: 443}, 0x1c)`
	lineConnectU = `D0615 10:20:30.150000      42 strace.go:567] [   1: 1] python3 E connect(0x3 socket:[12347], 0x7f4c0a1b3500 {Family: AF_UNIX, Path: /run/foo.sock}, 0x10)`
	lineNoise    = `D0615 10:20:30.160000      42 loader.go:123] Setting up application`
)

func TestParseStraceOpenat(t *testing.T) {
	evs := parseStraceEvents(strings.NewReader(lineOpenat))
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
	}
	ev := evs[0]
	if ev.Kind != "file_open" || ev.Syscall != "openat" {
		t.Errorf("kind/syscall = %q/%q, want file_open/openat", ev.Kind, ev.Syscall)
	}
	if ev.Path != "/usr/lib/python3.11/io.py" {
		t.Errorf("path = %q, want /usr/lib/python3.11/io.py", ev.Path)
	}
}

func TestParseStraceExecve(t *testing.T) {
	evs := parseStraceEvents(strings.NewReader(lineExecve))
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
	}
	ev := evs[0]
	if ev.Kind != "exec" || ev.Syscall != "execve" {
		t.Errorf("kind/syscall = %q/%q, want exec/execve", ev.Kind, ev.Syscall)
	}
	if ev.Path != "/usr/bin/python3" {
		t.Errorf("path = %q, want /usr/bin/python3", ev.Path)
	}
	want := []string{"python3", "solution.py"}
	if len(ev.Argv) != len(want) {
		t.Fatalf("argv = %v, want %v", ev.Argv, want)
	}
	for i := range want {
		if ev.Argv[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q (full %v)", i, ev.Argv[i], want[i], ev.Argv)
		}
	}
}

func TestParseStraceExecveat(t *testing.T) {
	evs := parseStraceEvents(strings.NewReader(lineExecveat))
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
	}
	ev := evs[0]
	if ev.Kind != "exec" || ev.Syscall != "execveat" {
		t.Errorf("kind/syscall = %q/%q, want exec/execveat", ev.Kind, ev.Syscall)
	}
	if ev.Path != "/bin/ls" {
		t.Errorf("path = %q, want /bin/ls", ev.Path)
	}
	want := []string{"ls", "-la"}
	if len(ev.Argv) != 2 || ev.Argv[0] != want[0] || ev.Argv[1] != want[1] {
		t.Errorf("argv = %v, want %v", ev.Argv, want)
	}
}

func TestParseStraceConnectIPv4(t *testing.T) {
	evs := parseStraceEvents(strings.NewReader(lineConnect4))
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
	}
	ev := evs[0]
	if ev.Kind != "connect" || ev.Syscall != "connect" {
		t.Errorf("kind/syscall = %q/%q, want connect/connect", ev.Kind, ev.Syscall)
	}
	if ev.DestIP != "8.8.8.8" || ev.DestPort != 53 {
		t.Errorf("dest = %s:%d, want 8.8.8.8:53", ev.DestIP, ev.DestPort)
	}
}

func TestParseStraceConnectIPv6(t *testing.T) {
	evs := parseStraceEvents(strings.NewReader(lineConnect6))
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
	}
	ev := evs[0]
	if ev.DestIP != "2001:4860:4860::8888" || ev.DestPort != 443 {
		t.Errorf("dest = %s:%d, want 2001:4860:4860::8888:443", ev.DestIP, ev.DestPort)
	}
}

// TestParseStraceSkips covers the lines that must produce NO event: exit ("X")
// lines (so each syscall is counted once, at entry — the eBPF sys_enter analogue),
// non-IP sockaddrs (AF_UNIX, mirroring the eBPF tracer's IP-only connect capture),
// and ordinary non-strace debug noise.
func TestParseStraceSkips(t *testing.T) {
	in := strings.Join([]string{lineOpenatX, lineConnectU, lineNoise, ""}, "\n")
	evs := parseStraceEvents(strings.NewReader(in))
	if len(evs) != 0 {
		t.Errorf("got %d events, want 0 (X line, AF_UNIX, and noise must all be skipped): %+v", len(evs), evs)
	}
}

// TestParseStraceEnterNotDuplicatedByExit feeds both the E and X line for the same
// openat and asserts exactly one event — the entry — so enabling strace does not
// double-count every syscall.
func TestParseStraceEnterNotDuplicatedByExit(t *testing.T) {
	in := strings.Join([]string{lineOpenat, lineOpenatX}, "\n")
	evs := parseStraceEvents(strings.NewReader(in))
	if len(evs) != 1 {
		t.Fatalf("got %d events, want exactly 1 (entry only, exit ignored): %+v", len(evs), evs)
	}
	if evs[0].Path != "/usr/lib/python3.11/io.py" {
		t.Errorf("path = %q, want the entry line's path", evs[0].Path)
	}
}

// TestParseStraceOrder pins that events come back in log (chronological) order
// across all three kinds, since the store preserves slice order.
func TestParseStraceOrder(t *testing.T) {
	in := strings.Join([]string{lineExecve, lineOpenat, lineConnect4}, "\n")
	evs := parseStraceEvents(strings.NewReader(in))
	wantKinds := []string{"exec", "file_open", "connect"}
	if len(evs) != len(wantKinds) {
		t.Fatalf("got %d events, want %d: %+v", len(evs), len(wantKinds), evs)
	}
	for i, k := range wantKinds {
		if evs[i].Kind != k {
			t.Errorf("event[%d].Kind = %q, want %q", i, evs[i].Kind, k)
		}
	}
}

// TestParseStraceArgvEscapes checks that a Go-quoted argv element with embedded
// spaces and an escaped quote is unquoted faithfully (the %q round-trip), not
// split on the inner space.
func TestParseStraceArgvEscapes(t *testing.T) {
	line := `D0615 10:20:30 42 strace.go:567] [1:1] sh E execve(0x1 /bin/sh, 0x2 ["sh", "-c", "echo \"a b\""], 0x3 ["PATH=/bin"])`
	evs := parseStraceEvents(strings.NewReader(line))
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
	}
	want := []string{"sh", "-c", `echo "a b"`}
	if len(evs[0].Argv) != len(want) {
		t.Fatalf("argv = %v, want %v", evs[0].Argv, want)
	}
	for i := range want {
		if evs[0].Argv[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, evs[0].Argv[i], want[i])
		}
	}
}

// TestCollectStraceEvents writes a fake boot log into a temp bundle dir and checks
// that collectStraceEvents finds it by the "boot" glob and parses its events,
// while ignoring sibling non-boot logs (e.g. the gofer log).
func TestCollectStraceEvents(t *testing.T) {
	dir := t.TempDir()
	bootLog := strings.Join([]string{lineExecve, lineOpenat, lineConnect4}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "runsc-boot.log"), []byte(bootLog), 0600); err != nil {
		t.Fatal(err)
	}
	// A non-boot component log must NOT contribute events.
	if err := os.WriteFile(filepath.Join(dir, "runsc-gofer.log"), []byte(lineConnect6), 0600); err != nil {
		t.Fatal(err)
	}
	evs := collectStraceEvents(dir)
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3 (boot log only): %+v", len(evs), evs)
	}
}

// TestCollectStraceEventsMissing confirms the best-effort contract: no boot log
// (strace off, or runsc died early) yields no events and no panic.
func TestCollectStraceEventsMissing(t *testing.T) {
	if evs := collectStraceEvents(t.TempDir()); evs != nil {
		t.Errorf("got %+v, want nil for a dir with no boot log", evs)
	}
}
