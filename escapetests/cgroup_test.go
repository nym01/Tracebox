//go:build escapetests

package escapetests

import (
	"strings"
	"testing"
)

// The cgroup group (tests 11-13) targets Phase 1's third pillar: the cgroup v2
// memory limit applied via nsjail's --cgroup_mem_max (with --cgroup_mem_swap_max 0
// so the limit is hard, not a soft spill-to-swap). Each language carries a
// memory_kb budget in configs/languages.yaml; the runner writes it to the child's
// memory.max, and a process whose RESIDENT footprint exceeds it is OOM-killed by
// SIGKILL, which nsjail surfaces as exit 137 and the API maps to memory_exceeded
// (see oomKilled in internal/runner/nsjail.go).
//
// Tests 11 and 12 probe two sides of that mechanism: a resident bomb that must be
// caught (11) and a zero-page allocation that, by design, is NOT resident and so
// documents a real boundary of memory.max accounting (12). Test 13 leaves the
// memory pillar entirely and targets process-count: the max_processes field in the
// YAML, now enforced as a cgroup v2 pids.max limit (--cgroup_pids_max). It was a
// real gap in the first pass of this suite — parsed and validated but never wired
// into the runner — and is now fixed: RunSpec carries MaxProcesses, handlers.go
// plumbs it from effectiveLimits, and buildNsjailArgs emits --cgroup_pids_max
// uniformly (see internal/runner/nsjail.go). Test 13 now asserts the bomb is
// BOUNDED at the configured count.

// Test 11 — resident-memory bomb (py3 and java).
//
// Attempts: allocate memory in chunks and TOUCH every page (write one byte per
// 4 KiB) so the pages become genuinely resident, looping until the cgroup limit is
// crossed. py3's budget is 100 MiB and java's is 512 MiB (configs/languages.yaml).
// Run for both: py3 is the simplest runtime, and java is the one Phase 1 singled
// out because the JVM reserves an enormous virtual address space up front, so
// --rlimit_as could never contain it — only the resident-memory cgroup limit can.
//
// Holds (expected): resident memory crosses memory.max, the kernel OOM-kills the
// child (SIGKILL → nsjail exit 137), and the API reports memory_exceeded. Some
// progress lines print first (each chunk is far smaller than the budget), then the
// process dies mid-allocation with no clean completion.
//
// Did NOT hold: the run reports accepted/wrong_output (the bomb finished — the
// limit did not bite) — a real failure of the memory pillar.
func TestMemoryBombResidentPy3(t *testing.T) {
	// 10 MiB chunks, each fully paged in; 100 MiB budget is crossed after ~10.
	const src = `
import sys
chunks = []
total = 0
while True:
    b = bytearray(10 * 1024 * 1024)
    for i in range(0, len(b), 4096):
        b[i] = 1
    chunks.append(b)
    total += len(b)
    print(total // (1024 * 1024), "MiB", flush=True)
`
	run := runPy3(t, src)
	t.Logf("py3 resident bomb: status=%s dur=%dms stdout=%q stderr=%q",
		run.Status, run.DurationMs, lastLines(run.Stdout, 3), run.Stderr)
	assertMemoryExceeded(t, "py3 resident bomb", run)
}

func TestMemoryBombResidentJava(t *testing.T) {
	// 25 MiB chunks, each page touched; 512 MiB budget is crossed after ~20.
	const src = `
import java.util.ArrayList;
import java.util.List;
public class Main {
    public static void main(String[] args) {
        List<byte[]> chunks = new ArrayList<>();
        long total = 0;
        while (true) {
            byte[] b = new byte[25 * 1024 * 1024];
            for (int i = 0; i < b.length; i += 4096) b[i] = 1;
            chunks.add(b);
            total += b.length;
            System.out.println((total / (1024 * 1024)) + " MiB");
            System.out.flush();
        }
    }
}
`
	resp := runJava(t, src, "Main", "")
	build := resp.Build
	run := resp.Tests[0]
	t.Logf("java resident bomb: build=%s run=%s dur=%dms stdout=%q stderr=%q",
		build.Status, run.Status, run.DurationMs, lastLines(run.Stdout, 3), run.Stderr)
	if build.Status != "ok" {
		t.Fatalf("java resident bomb: build did not succeed (status %q, stderr %q)", build.Status, build.Stderr)
	}
	assertMemoryExceeded(t, "java resident bomb", run)
}

// Test 12 — zero-page trick: a non-resident allocation an order of magnitude over
// the budget that the cgroup limit does NOT catch.
//
// Attempts: anonymously mmap 1 GiB (10x py3's 100 MiB budget) with
// mmap.mmap(-1, size) — a lazily zero-filled mapping, NOT eagerly memset like
// bytearray(n) would be — then walk it READ-ONLY, reading one byte per 4 KiB page.
// Reading a never-written anonymous page faults in the kernel's shared zero page,
// which cgroup v2 does not charge to the process's memory.max, so resident memory
// stays near zero no matter how large the mapping. This is the exact edge case
// Phase 1 hit while debugging: a "memory bomb" that allocates far more than the
// budget yet is never OOM-killed, because memory.max accounts RESIDENT pages and
// these pages never become resident.
//
// This is NOT a held/failed assertion about the sandbox — it documents a real
// BOUNDARY of the cgroup approach. memory.max is a resident-memory limit, so it
// does exactly what it says and nothing more: it bounds physical footprint, not
// virtual address space or untouched allocations. The companion fact (proven by
// test 11) is that the moment those same pages are WRITTEN they become resident and
// the limit fires immediately; the only thing that slips through is memory you
// allocate but never actually use.
//
// Documented outcome: the program completes (no memory_exceeded), because nothing
// it did consumed resident memory. The XOR of zero bytes is 0, so output is
// deterministic and the verdict is a clean accepted. If this ever reported
// memory_exceeded instead, the accounting would have changed (zero pages being
// charged) — worth knowing, but not the current behaviour.
func TestMemoryBombZeroPage(t *testing.T) {
	const sizeMiB = 1024 // 1 GiB, ~10x py3's 100 MiB budget
	// MAP_PRIVATE is the crucial flag. mmap.mmap(-1, size) defaults to MAP_SHARED,
	// and anonymous MAP_SHARED pages are real shmem pages charged to memory.max the
	// instant they are faulted in — even by a READ — so the naive zero-buffer attempt
	// is actually OOM-killed (verified empirically). Only MAP_PRIVATE gives the true
	// zero-page behaviour: a read fault on a never-written private page maps the
	// kernel's shared zero page, which is not charged, so resident memory stays near
	// zero however large the mapping. This is the distinction the boundary turns on.
	const src = `
import mmap
size = ` + "1024 * 1024 * 1024" + `
m = mmap.mmap(-1, size, flags=mmap.MAP_PRIVATE)
print("MMAP_OK", size // (1024 * 1024), "MiB", flush=True)
acc = 0
for i in range(0, size, 4096):
    acc ^= m[i]
print("READ_DONE", acc, flush=True)
`
	resp := submit(t, runRequest{
		Language: "py3",
		Source:   src,
		Tests:    []testCase{{Stdin: "", ExpectedStdout: "MMAP_OK 1024 MiB\nREAD_DONE 0\n"}},
	})
	run := resp.Tests[0]
	t.Logf("py3 zero-page (%d MiB, read-only): status=%s dur=%dms stdout=%q stderr=%q",
		sizeMiB, run.Status, run.DurationMs, run.Stdout, run.Stderr)

	switch run.Status {
	case "memory_exceeded":
		t.Errorf("zero-page: reported memory_exceeded — untouched zero pages were charged to memory.max, "+
			"which contradicts the documented boundary; stdout=%q", run.Stdout)
	case "time_exceeded":
		t.Logf("zero-page: time_exceeded — the read-only walk of %d MiB did not OOM but the wall clock "+
			"caught it; the cgroup limit still did not fire (the documented point)", sizeMiB)
	case "accepted":
		// documented outcome: a >budget allocation sailed through because it never
		// became resident.
	default:
		t.Logf("zero-page: status %q — did not OOM (the documented point); stdout=%q stderr=%q",
			run.Status, run.Stdout, run.Stderr)
	}
	// The one thing that must be true for the boundary to be demonstrated: the
	// allocation was NOT stopped by the memory limit.
	if run.Status == "memory_exceeded" {
		t.FailNow()
	}
	if !strings.Contains(run.Stdout, "READ_DONE 0") {
		t.Errorf("zero-page: program did not finish the read-only walk; expected READ_DONE; stdout=%q stderr=%q",
			run.Stdout, run.Stderr)
	}
}

// Test 13 — fork bomb: process-count limiting (max_processes) IS enforced.
//
// This test leaves the memory pillar and targets process COUNT. Every language in
// configs/languages.yaml carries a max_processes budget (c's run step: 64), enforced
// as a cgroup v2 pids.max limit: RunSpec carries MaxProcesses, handlers.go plumbs it
// from effectiveLimits, and buildNsjailArgs emits --cgroup_pids_max (the vendored
// nsjail writes pids.max in external/nsjail/cgroup2.cc).
//
// How the limit surfaces is the key subtlety. Unlike the memory limit — where the
// kernel OOM-kills the child (SIGKILL → nsjail exit 137 → memory_exceeded) — hitting
// pids.max does NOT kill anything: the kernel simply fails the next fork()/clone()
// with EAGAIN. The sandboxed program sees that failed syscall and decides what to do.
// This program treats a failed fork as fatal and exits NON-ZERO, so the API maps it
// to runtime_error (there is no dedicated "process_limit_exceeded" status, and the
// runner could not infer one: an EAGAIN-from-pids is indistinguishable from any other
// non-zero exit — there is no signal like OOM's 137 to key off).
//
// Holds (expected): fork() fails with EAGAIN once the cgroup reaches pids.max, far
// below the self-cap — c's run budget is 64, and the cgroup holds the run process
// plus its children, so the failure lands at created≈63 (1 parent + 63 children =
// 64 tasks). The program prints FORK_FAILED with errno=11 (EAGAIN) and exits 1 →
// runtime_error. CAP_REACHED must NOT appear — reaching the self-cap would mean the
// limit never fired.
//
// What also HOLDS is containment of blast radius: the sandbox runs in its own PID
// namespace (Group 1, test 5), so even the bounded children die with the namespace
// when nsjail exits. The test confirms the container survives (a trivial run still
// succeeds afterwards).
//
// The fork bomb is deliberately BOUNDED: only the parent forks (children just sleep,
// they do not recursively fork, so growth is linear not exponential), and it self-caps
// at forkBombCap — far above c's 64 budget. With max_processes enforced the cap is now
// unreachable; reaching it would be the failure signal that the limit is missing.
func TestForkBombProcessLimit(t *testing.T) {
	const forkBombCap = 2000    // >> c-run max_processes (64); must NOT be reached now
	const cRunMaxProcesses = 64 // configs/languages.yaml: c.run.limits.max_processes
	const src = `
#include <stdio.h>
#include <unistd.h>
#include <errno.h>
#include <string.h>
int main(void){
    int created = 0;
    while (created < ` + "2000" + `) {
        pid_t p = fork();
        if (p < 0) {
            // pids.max reached: fork() returns EAGAIN. Treat it as fatal and exit
            // NON-ZERO so the run is reported as runtime_error (the process is not
            // killed — the kernel just refuses the syscall).
            printf("FORK_FAILED created=%d errno=%d(%s)\n", created, errno, strerror(errno));
            fflush(stdout);
            return 1;
        }
        if (p == 0) {
            // Child: hold a process slot, then exit. The run's wall limit kills the
            // whole PID namespace well before this returns, so lifetime is bounded.
            sleep(30);
            _exit(0);
        }
        created++;
        if (created % 100 == 0) { printf("created=%d\n", created); fflush(stdout); }
    }
    printf("CAP_REACHED created=%d\n", created);
    fflush(stdout);
    return 0;
}
`
	resp := runC(t, src, "")
	build := resp.Build
	run := resp.Tests[0]
	t.Logf("fork bomb: build=%s run=%s dur=%dms stdout-tail=%q stderr=%q",
		build.Status, run.Status, run.DurationMs, lastLines(run.Stdout, 3), run.Stderr)
	if build.Status != "ok" {
		t.Fatalf("fork bomb: C build did not succeed (status %q, stderr %q)", build.Status, build.Stderr)
	}

	// The bomb must NOT have reached its self-cap: that would mean nothing limited the
	// process count (the original gap). With pids.max enforced this is impossible.
	if strings.Contains(run.Stdout, "CAP_REACHED") {
		t.Fatalf("REGRESSION: the bomb reached its self-cap of %d processes — max_processes is NOT "+
			"enforced (no pids.max limit fired below %d). stdout-tail=%q",
			forkBombCap, forkBombCap, lastLines(run.Stdout, 3))
	}

	// fork() must have failed (EAGAIN from pids.max), and the program exited non-zero,
	// so the run is runtime_error. A timeout/OOM here would mean the count was bounded
	// only incidentally rather than by the configured limit — surface that distinction.
	switch run.Status {
	case "runtime_error":
		// expected: fork() hit pids.max and the program exited 1.
	case "memory_exceeded":
		t.Errorf("the bomb was stopped by the MEMORY limit, not the process-count limit — "+
			"max_processes did not fire first; stdout-tail=%q", lastLines(run.Stdout, 3))
	case "time_exceeded":
		t.Errorf("the bomb ran until the wall-clock limit rather than hitting pids.max — " +
			"the process-count limit did not fire first")
	default:
		t.Errorf("expected runtime_error (fork() → EAGAIN at pids.max, exit 1), got %q; stdout-tail=%q stderr=%q",
			run.Status, lastLines(run.Stdout, 3), run.Stderr)
	}

	// The bound must be the CONFIGURED process count, not some incidental host limit.
	// A clean pids.max=64 surfaces as fork() failing at created≈63 with errno 11
	// (EAGAIN). Assert FORK_FAILED is present, the errno is EAGAIN, and the count is
	// in the neighbourhood of the configured 64 (well below the self-cap).
	if !strings.Contains(run.Stdout, "FORK_FAILED") {
		t.Fatalf("expected FORK_FAILED in output (fork() should hit pids.max); stdout=%q", run.Stdout)
	}
	if !strings.Contains(run.Stdout, "errno=11") {
		t.Errorf("expected fork() to fail with EAGAIN (errno=11) from pids.max, got a different errno; stdout-tail=%q",
			lastLines(run.Stdout, 3))
	}
	var failedAt int
	if i := strings.Index(run.Stdout, "FORK_FAILED created="); i >= 0 {
		_, _ = fmtSscanCreated(run.Stdout[i:], &failedAt)
	}
	// Allow a small margin around the configured 64: the cgroup counts the run process
	// itself plus a possible transient, so the exact failing count can be a hair under
	// or over 64, but it must be nowhere near the 2000 self-cap.
	if failedAt <= 0 || failedAt > cRunMaxProcesses+16 {
		t.Errorf("fork failed at created=%d, but the configured limit is max_processes=%d — the bound "+
			"does not match the per-language process count (expected ~%d, far below the %d self-cap)",
			failedAt, cRunMaxProcesses, cRunMaxProcesses, forkBombCap)
	} else {
		t.Logf("BOUNDED: fork() failed at created=%d (EAGAIN), matching c's max_processes=%d — pids.max enforced.",
			failedAt, cRunMaxProcesses)
	}

	// Containment check: the bomb must not have taken the service down. A trivial run
	// afterwards must still succeed — proving the PID namespace + wall limit contained
	// the blast radius even though the process count itself was unbounded.
	after := runPy3(t, "print('alive')")
	if !strings.Contains(after.Stdout, "alive") {
		t.Errorf("CONTAINMENT FAILURE: the service did not survive the fork bomb — a follow-up run "+
			"did not return normally (status %q, stdout %q)", after.Status, after.Stdout)
	} else {
		t.Logf("containment OK: service responsive after the bomb (follow-up run returned %q)", strings.TrimSpace(after.Stdout))
	}
}

// fmtSscanCreated extracts N from a string beginning "FORK_FAILED created=N". Kept
// tiny and local; avoids pulling fmt.Sscanf semantics into the assertion inline.
func fmtSscanCreated(s string, out *int) (int, error) {
	const prefix = "FORK_FAILED created="
	s = strings.TrimPrefix(s, prefix)
	n := 0
	read := 0
	for read < len(s) && s[read] >= '0' && s[read] <= '9' {
		n = n*10 + int(s[read]-'0')
		read++
	}
	*out = n
	return read, nil
}

// assertMemoryExceeded asserts the standard shape of a run stopped by the cgroup
// memory limit: status memory_exceeded. Anything else is logged as the specific
// alternative so a surprising outcome (the JVM throwing its own OutOfMemoryError →
// runtime_error, say, instead of being OOM-killed by the cgroup) is visible rather
// than hidden behind a bare failure.
func assertMemoryExceeded(t *testing.T, label string, run testResult) {
	t.Helper()
	switch run.Status {
	case "memory_exceeded":
		// held (expected)
	case "accepted", "wrong_output":
		t.Errorf("%s: the bomb COMPLETED (status %q) — the cgroup memory limit did not bite; stdout tail=%q",
			label, run.Status, lastLines(run.Stdout, 3))
	default:
		t.Errorf("%s: expected memory_exceeded, got %q — the process was stopped, but not by the cgroup OOM killer; stderr=%q",
			label, run.Status, run.Stderr)
	}
}

// lastLines returns the final n non-empty lines of s, for compact logging of a
// bomb's progress output without dumping hundreds of lines.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
