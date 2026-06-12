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
// memory pillar entirely and asks whether process-count is limited at all — the
// max_processes field in the YAML — which turns out to be a real gap.

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

// Test 13 — fork bomb: process-count limiting (max_processes) is NOT enforced.
//
// This test leaves the memory pillar and targets process COUNT. Every language in
// configs/languages.yaml carries a max_processes budget (c's run step: 64), and
// the intent was clearly to cap the number of processes a submission can spawn.
//
// A pre-run audit of the code shows that intent was never connected to any
// enforcement:
//   - max_processes IS parsed (internal/language/loader.go) and even validated on
//     a per-request override (internal/api/handlers.go effectiveLimits),
//   - but runner.RunSpec has NO MaxProcesses field — only WallTimeSec and MemoryKB —
//     so handlers.go drops the value when it builds the spec, and
//   - buildNsjailArgs (internal/runner/nsjail.go) emits no --cgroup_pids_max and no
//     --rlimit_nproc. The vendored nsjail fully supports --cgroup_pids_max
//     (external/nsjail/cgroup2.cc writes pids.max); the runner simply never passes it.
//
// So max_processes is dead config. The PREDICTION (confirmed by the run below) is
// that a fork bomb is NOT cleanly capped at 64; it is bounded only incidentally —
// by the cgroup MEMORY limit (each retained child costs memory → memory_exceeded),
// the wall-clock --time_limit (→ time_exceeded), or nsjail's inherited soft
// RLIMIT_NPROC. None of those is the configured per-language process count.
//
// What still HOLDS is containment of blast radius, not the count limit: the sandbox
// runs in its own PID namespace (Group 1, test 5), so when nsjail exits or hits the
// wall limit it tears the namespace down and every spawned child dies with it — the
// bomb cannot outlive the request or reach host processes. The test confirms the
// container survives (a trivial run still succeeds afterwards).
//
// The fork bomb is deliberately BOUNDED: only the parent forks (children just
// sleep, they do not recursively fork, so growth is linear not exponential), and it
// self-caps at forkBombCap — far above c's 64 budget, low enough not to threaten the
// host's task table, and moot anyway because the wall limit kills the namespace in a
// few seconds. Reaching the cap, or being stopped by memory/time, both prove the
// same thing: nothing capped it at 64.
func TestForkBombProcessLimit(t *testing.T) {
	const forkBombCap = 2000 // >> c-run max_processes (64); children die with the PID ns
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
            printf("FORK_FAILED created=%d errno=%d(%s)\n", created, errno, strerror(errno));
            fflush(stdout);
            return 0;
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

	// Interpret which mechanism (if any) stopped the bomb. The point of the test is
	// to record this honestly, not to assert a single "correct" status — there is a
	// real gap here, so the finding is the result.
	switch run.Status {
	case "memory_exceeded":
		t.Logf("FINDING: stopped by the cgroup MEMORY limit, not a process-count limit — "+
			"the retained children's memory tripped memory.max. max_processes (64) played no role. stdout-tail=%q",
			lastLines(run.Stdout, 3))
	case "time_exceeded":
		t.Logf("FINDING: stopped by the wall-clock --time_limit, not a process-count limit — " +
			"forking ran until the deadline. max_processes (64) played no role.")
	case "runtime_error":
		// fork() eventually returned <0. Confirm it was NOT the configured 64 — i.e.
		// the bound was the host's RLIMIT_NPROC/pid_max, not max_processes.
		t.Logf("FINDING: fork() eventually failed (RLIMIT_NPROC / host pid_max), not the configured "+
			"max_processes=64. stdout-tail=%q", lastLines(run.Stdout, 3))
		if strings.Contains(run.Stdout, "FORK_FAILED created=6") && !strings.Contains(run.Stdout, "created=60") {
			t.Logf("note: fork failed near a low count — inspect whether a limit near 64 was hit")
		}
	case "accepted", "wrong_output":
		t.Logf("FINDING: the bomb reached its self-cap of %d processes with NO failure — there is no "+
			"process-count limit at all below %d. max_processes=64 is unenforced. stdout-tail=%q",
			forkBombCap, forkBombCap, lastLines(run.Stdout, 3))
	}

	// The actual gap assertion: a clean process-count limit at the configured value
	// would have surfaced as fork() failing with EAGAIN at ~64 (a runtime_error whose
	// output shows created≈64). If we ever see exactly that, max_processes has been
	// wired up and THIS test's finding (and docs/security.md) must be updated.
	if strings.Contains(run.Stdout, "FORK_FAILED") {
		var failedAt int
		// Parse "FORK_FAILED created=N" loosely.
		if i := strings.Index(run.Stdout, "FORK_FAILED created="); i >= 0 {
			_, _ = fmtSscanCreated(run.Stdout[i:], &failedAt)
		}
		if failedAt > 0 && failedAt <= 120 {
			t.Errorf("UNEXPECTED: fork failed at created=%d, near c's max_processes=64 — a process-count "+
				"limit may now be enforced. If so this is GOOD, but the test's finding and docs/security.md "+
				"must be updated to reflect that max_processes is now wired up.", failedAt)
		}
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
