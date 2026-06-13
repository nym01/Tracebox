//go:build escapetests

package escapetests

import (
	"strings"
	"testing"
)

// The seccomp group (tests 6-10) targets Phase 1's second pillar: the kafel
// deny-list at configs/seccomp.policy, applied UNIFORMLY to every language via
// nsjail's --seccomp_policy. The policy KILLs (SECCOMP_RET_KILL → SIGSYS) a fixed
// set of escape/host-tampering syscalls — ptrace, bpf, mount, umount, kexec_load,
// the module syscalls, reboot, swapon/swapoff, setns, and unshare when its flags
// request a new user or mount namespace — and ALLOWs everything else by default.
//
// These tests use C, not py3: C is the only one of the seven runtimes that can
// issue a raw syscall directly. The interpreted runtimes can't even reach most of
// these — py3 has no ctypes (libffi is not in its mount profile) and no language
// binds a `mount` binary — so the seccomp filter sits behind an already-narrow
// attack surface. C goes straight at the syscall, which is exactly what the
// deny-list is there to stop.
//
// Each program prints a "BEFORE" marker and flushes, issues the denied syscall,
// then prints "AFTER". A SIGSYS kill terminates the process the instant it makes
// the call, so a blocked attempt shows "BEFORE" (already flushed) but never
// "AFTER", and the API reports the run as runtime_error. assertSeccompKilled
// checks that shape; seeing "AFTER" would mean the syscall slipped through.

// Test 6 — ptrace(PTRACE_TRACEME).
//
// Attempts: ptrace(PTRACE_TRACEME) — the canonical sandbox-escape / anti-analysis
// primitive (attach to and control another process's memory and execution).
// Proven informally in Phase 1 with a C program; formalized here.
//
// Holds (expected): the seccomp policy KILLs ptrace, so the process dies by SIGSYS
// at the call — "BEFORE" prints, "AFTER" does not, run status is runtime_error.
//
// Did NOT hold: "AFTER ptrace=0" prints (the call returned, so ptrace was allowed).
func TestSeccompPtrace(t *testing.T) {
	const src = `
#include <stdio.h>
#include <sys/ptrace.h>
int main(void){
    printf("BEFORE\n");
    fflush(stdout);
    long r = ptrace(PTRACE_TRACEME, 0, 0, 0);
    printf("AFTER ptrace=%ld\n", r);
    fflush(stdout);
    return 0;
}
`
	resp := runC(t, src, "")
	assertSeccompKilled(t, resp, "ptrace")
}

// Test 7 — unshare(CLONE_NEWUSER).
//
// Attempts: unshare(CLONE_NEWUSER) — create a new user namespace, the classic
// privilege-escalation primitive (it hands the caller a full capability set
// inside the new namespace, which can then be leveraged to undo isolation).
// Proven informally in Phase 1; formalized here.
//
// Holds (expected): the policy KILLs unshare specifically when its flags include
// CLONE_NEWUSER or CLONE_NEWNS (a flag-conditional rule, so harmless unshare flags
// stay allowed). CLONE_NEWUSER matches, so the process is SIGSYS-killed at the
// call — "BEFORE" only, runtime_error.
//
// Did NOT hold: "AFTER unshare=0" prints (a new user namespace was created).
func TestSeccompUnshareNewuser(t *testing.T) {
	const src = `
#define _GNU_SOURCE
#include <stdio.h>
#include <sched.h>
int main(void){
    printf("BEFORE\n");
    fflush(stdout);
    int r = unshare(CLONE_NEWUSER);
    printf("AFTER unshare=%d\n", r);
    fflush(stdout);
    return 0;
}
`
	resp := runC(t, src, "")
	assertSeccompKilled(t, resp, "unshare(CLONE_NEWUSER)")
}

// Test 8 — umount2 a bind mount.
//
// Attempts: umount2() on the per-request work directory — itself a bind mount.
// Detaching it (or, via mount(2), remounting a read-only bind mount read-write)
// is exactly how a program would try to undo the filesystem isolation from the
// inside, which is why mount/umount sit on the deny-list. The work dir's path is
// discovered at runtime via getcwd so the attempt names a mount that genuinely
// exists in the sandbox.
//
// This is the one seccomp test that, per the notes, should use "whatever language
// is simplest" rather than C. It still uses C — and that itself is the finding:
// none of the high-level runtimes can reach the mount syscalls at all. py3's
// ctypes import fails (libffi.so.8 is not in its mount profile) and Python's os
// module has no mount(); bash has no mount builtin and no mount binary is bound.
// So C is the simplest language that can even attempt this — the mount syscalls
// are unreachable from the interpreted languages before seccomp is even consulted.
//
// Holds (expected): the policy KILLs umount (kafel's name for amd64 umount2, no.
// 166), so the process is SIGSYS-killed at the call — "BEFORE cwd=/tmp/goboxd-..."
// only, runtime_error.
//
// Did NOT hold: "AFTER umount2=..." prints (the call returned — the work-dir mount
// could be manipulated from inside the sandbox).
func TestSeccompUmount(t *testing.T) {
	const src = `
#define _GNU_SOURCE
#include <stdio.h>
#include <sys/mount.h>
#include <unistd.h>
int main(void){
    char cwd[4096];
    if (!getcwd(cwd, sizeof(cwd))) cwd[0] = 0;
    printf("BEFORE cwd=%s\n", cwd);
    fflush(stdout);
    int r = umount2(cwd, 0);
    printf("AFTER umount2=%d\n", r);
    fflush(stdout);
    return 0;
}
`
	resp := runC(t, src, "")
	assertSeccompKilled(t, resp, "umount2")
	// The work dir really is a bind mount, so the program must have gotten as far
	// as resolving it before the syscall — a sanity check that we killed umount2 on
	// a genuine target rather than failing earlier.
	if !strings.Contains(resp.Tests[0].Stdout, "cwd=/tmp/goboxd-") {
		t.Errorf("expected the work-dir path in the BEFORE marker; stdout=%q", resp.Tests[0].Stdout)
	}
}

// Test 9 — setns.
//
// Attempts: open /proc/self/ns/mnt and call setns() on the resulting fd — the way
// a program steps out of its own namespaces into another (e.g. the host's). The
// open is expected to succeed (procfs is mounted and the ns link is the process's
// own); setns is the syscall the deny-list must stop.
//
// Holds (expected): opening the ns fd succeeds (fd >= 0, printed in the BEFORE
// marker), then the policy KILLs setns, so the process is SIGSYS-killed at the
// call — "BEFORE fd=3" only, runtime_error. That the open works but setns does not
// is the point: the namespace handle is reachable, but joining a namespace is not.
//
// Did NOT hold: "AFTER setns=..." prints (the call returned — a namespace was
// joined, or attempted, without the process being killed).
func TestSeccompSetns(t *testing.T) {
	const src = `
#define _GNU_SOURCE
#include <stdio.h>
#include <sched.h>
#include <fcntl.h>
#include <unistd.h>
int main(void){
    int fd = open("/proc/self/ns/mnt", O_RDONLY);
    printf("BEFORE fd=%d\n", fd);
    fflush(stdout);
    int r = setns(fd, 0);
    printf("AFTER setns=%d\n", r);
    fflush(stdout);
    return 0;
}
`
	resp := runC(t, src, "")
	assertSeccompKilled(t, resp, "setns")
}

// Test 10 — fork/clone is NOT on the deny-list (negative control).
//
// Attempts: fork() a child, have it _exit(7), and wait for it. fork is implemented
// via the clone syscall on Linux, and clone is deliberately ALLOWed — the
// compiled-language build steps shell out to sub-processes, so blocking it would
// break normal operation. This is the negative test that proves the deny-list is
// not accidentally too broad: a syscall that is NOT dangerous-by-name must keep
// working.
//
// Holds (expected): the child runs and exits 7, the parent reaps it and prints
// FORK_OK, the program exits 0, and (with expected_stdout matching) the verdict is
// accepted — fork/clone was not blocked.
//
// Did NOT hold (for THIS test's purpose): the run is runtime_error, i.e. fork was
// killed — which would mean the deny-list (or the sandbox) is over-restrictive and
// breaks legitimate process creation.
func TestSeccompForkAllowed(t *testing.T) {
	const src = `
#include <stdio.h>
#include <unistd.h>
#include <sys/wait.h>
int main(void){
    pid_t p = fork();
    if (p == 0) { _exit(7); }
    if (p < 0) { printf("FORK_FAIL\n"); return 1; }
    int st = 0;
    waitpid(p, &st, 0);
    if (WIFEXITED(st) && WEXITSTATUS(st) == 7)
        printf("FORK_OK\n");
    else
        printf("FORK_WEIRD\n");
    return 0;
}
`
	resp := runC(t, src, "FORK_OK\n")
	build := resp.Build
	run := resp.Tests[0]
	t.Logf("fork: build=%s run=%s stdout=%q stderr=%q", build.Status, run.Status, run.Stdout, run.Stderr)

	if build.Status != "ok" {
		t.Fatalf("fork: C build did not succeed (status %q, stderr %q)", build.Status, build.Stderr)
	}
	if run.Status == "runtime_error" {
		t.Errorf("fork/clone was blocked — the deny-list is over-restrictive and breaks legitimate process creation; stdout=%q stderr=%q", run.Stdout, run.Stderr)
	}
	if !strings.Contains(run.Stdout, "FORK_OK") {
		t.Errorf("fork did not complete normally; expected FORK_OK; stdout=%q stderr=%q", run.Stdout, run.Stderr)
	}
	// With expected_stdout matching exactly, a working fork yields a clean accepted.
	if run.Status != "accepted" {
		t.Errorf("expected accepted for an allowed syscall, got %q; stdout=%q", run.Status, run.Stdout)
	}
}

// Test 16 — clone / clone3 cannot create a new user namespace (audit Finding A).
//
// Background: the deny-list filtered user-/mount-namespace creation only on the
// `unshare` syscall. But clone(CLONE_NEWUSER, …) and clone3(.flags=CLONE_NEWUSER)
// create exactly the same namespace, and a new user namespace hands the caller a
// FULL capability set (CapEff/CapPrm/CapBnd = 0x1ffffffffff, including
// CAP_SYS_ADMIN) inside it — falsifying the "capability-less, CapBnd empty"
// property test 14 relies on. The red-team audit demonstrated this live: a C
// program calling clone(CLONE_NEWUSER) read a non-empty CapEff from the child's
// /proc/self/status. This test reproduces that gap and asserts it is now closed.
//
// The fix (configs/seccomp.policy):
//   - clone is arg-filtered on clone_flags exactly like unshare: a call requesting
//     CLONE_NEWUSER (or CLONE_NEWNS) is SIGSYS-KILLed. Ordinary process/thread
//     creation, which never sets those flags, stays allowed (test 10 still passes).
//   - clone3 hides its flags behind a struct pointer seccomp cannot dereference, so
//     it cannot be arg-filtered. It is given ERRNO(ENOSYS) instead of KILL, which
//     makes glibc fall back to the (filtered) clone syscall and makes a direct
//     clone3(CLONE_NEWUSER) simply fail with ENOSYS — no namespace either way.
//
// The program probes both paths in one run:
//   (1) clone3(CLONE_NEWUSER) via raw syscall — must FAIL with ENOSYS, not return a
//       child holding capabilities (CLONE3_USERNS_OK would be the escape).
//   (2) clone(CLONE_NEWUSER|SIGCHLD) — must be SIGSYS-KILLed, so the process dies
//       here: neither CLONE_USERNS_OK (the child ran with regained caps) nor the
//       trailing AFTER marker may appear, and the run is runtime_error.
//
// Holds (expected): BEFORE prints; clone3 returns -1/ENOSYS (CLONE3_RET=-1 ...
// errno=38), so no user namespace; then clone is KILLed — no CLONE_USERNS_OK, no
// AFTER, run status runtime_error.
//
// Did NOT hold (ESCAPE): either CLONE3_USERNS_OK or CLONE_USERNS_OK prints (a user
// namespace was created and the child reported a non-empty capability set), or
// AFTER prints (clone returned instead of being killed).
func TestSeccompCloneNewuserBlocked(t *testing.T) {
	const src = `
#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>
#include <sched.h>
#include <unistd.h>
#include <sys/syscall.h>
#include <sys/wait.h>

#ifndef __NR_clone3
#define __NR_clone3 435
#endif

/* Minimal clone_args, declared locally so the test does not depend on the host's
   <linux/sched.h> carrying struct clone_args. */
struct tb_clone_args {
    unsigned long long flags;
    unsigned long long pidfd;
    unsigned long long child_tid;
    unsigned long long parent_tid;
    unsigned long long exit_signal;
    unsigned long long stack;
    unsigned long long stack_size;
    unsigned long long tls;
};

/* Read this task's CapEff line from /proc/self/status (no trailing newline). */
static void read_capeff(char *buf, size_t n) {
    buf[0] = 0;
    FILE *f = fopen("/proc/self/status", "r");
    if (!f) return;
    char line[256];
    while (fgets(line, sizeof(line), f)) {
        if (strncmp(line, "CapEff:", 7) == 0) {
            size_t len = strlen(line);
            while (len > 0 && (line[len-1] == '\n' || line[len-1] == '\r')) line[--len] = 0;
            snprintf(buf, n, "%s", line);
            break;
        }
    }
    fclose(f);
}

/* clone(2) child: reaching here means CLONE_NEWUSER succeeded — the escape. */
static int child_fn(void *arg) {
    char cap[256];
    read_capeff(cap, sizeof(cap));
    printf("CLONE_USERNS_OK %s\n", cap);
    fflush(stdout);
    return 0;
}

int main(void) {
    printf("BEFORE\n");
    fflush(stdout);

    /* (1) clone3(CLONE_NEWUSER) via raw syscall. */
    struct tb_clone_args ca;
    memset(&ca, 0, sizeof(ca));
    ca.flags = 0x10000000ULL; /* CLONE_NEWUSER */
    ca.exit_signal = SIGCHLD;
    long c3 = syscall(__NR_clone3, &ca, sizeof(ca));
    if (c3 == 0) {
        /* Child of a successful clone3 — user namespace was created (escape). */
        char cap[256];
        read_capeff(cap, sizeof(cap));
        printf("CLONE3_USERNS_OK %s\n", cap);
        fflush(stdout);
        _exit(0);
    }
    printf("CLONE3_RET=%ld errno=%d\n", c3, errno);
    fflush(stdout);

    /* (2) clone(CLONE_NEWUSER|SIGCHLD) — expected to be SIGSYS-killed here. */
    char *stack = malloc(1 << 20);
    if (!stack) { printf("MALLOC_FAIL\n"); return 1; }
    pid_t pid = clone(child_fn, stack + (1 << 20), CLONE_NEWUSER | SIGCHLD, NULL);
    if (pid > 0) waitpid(pid, NULL, 0);
    printf("AFTER clone=%d\n", pid);
    fflush(stdout);
    return 0;
}
`
	resp := runC(t, src, "")
	build := resp.Build
	run := resp.Tests[0]
	t.Logf("clone-newuser: build=%s run=%s stdout=%q stderr=%q", build.Status, run.Status, run.Stdout, run.Stderr)

	if build.Status != "ok" {
		t.Fatalf("clone-newuser: C build did not succeed (status %q, stderr %q) — cannot conclude anything", build.Status, build.Stderr)
	}
	if !strings.Contains(run.Stdout, "BEFORE") {
		t.Errorf("program did not reach the BEFORE marker — the kill is unproven; stdout=%q stderr=%q", run.Stdout, run.Stderr)
	}
	// The headline escape: a new user namespace was created and the child reported
	// its (now non-empty) capability set. Either marker means Finding A is open.
	if strings.Contains(run.Stdout, "CLONE3_USERNS_OK") {
		t.Errorf("ESCAPE: clone3(CLONE_NEWUSER) created a user namespace — child regained capabilities; stdout=%q", run.Stdout)
	}
	if strings.Contains(run.Stdout, "CLONE_USERNS_OK") {
		t.Errorf("ESCAPE: clone(CLONE_NEWUSER) created a user namespace — child regained capabilities; stdout=%q", run.Stdout)
	}
	// clone3 must have been refused with ENOSYS (errno 38), proving the ENOSYS
	// fallback rule fired rather than clone3 succeeding.
	if !strings.Contains(run.Stdout, "CLONE3_RET=-1") || !strings.Contains(run.Stdout, "errno=38") {
		t.Errorf("clone3(CLONE_NEWUSER) was not refused with ENOSYS as expected; stdout=%q", run.Stdout)
	}
	// clone(CLONE_NEWUSER) must have been SIGSYS-killed, so AFTER never prints and
	// the run is runtime_error.
	if strings.Contains(run.Stdout, "AFTER") {
		t.Errorf("ESCAPE: clone(CLONE_NEWUSER) returned instead of being killed — the arg-filter did not block it; stdout=%q", run.Stdout)
	}
	if run.Status != "runtime_error" {
		t.Errorf("expected run status runtime_error (SIGSYS kill at clone), got %q; stdout=%q stderr=%q", run.Status, run.Stdout, run.Stderr)
	}
}

// Test 17 — io_uring cannot be set up (audit Finding B).
//
// Background: io_uring (syscalls 425-427) is a modern async-I/O interface whose
// submitted operations execute on KERNEL worker threads, not as syscalls from the
// sandboxed task — so a per-task seccomp filter cannot see them. The audit found it
// fully available (a C program got "IO_URING_OK fd=3"): a large kernel attack
// surface (a prolific LPE-CVE source) and a latent channel that could end-run any
// future tightening of the deny-list. This test reproduces that probe and asserts
// it is now closed.
//
// The fix (configs/seccomp.policy): io_uring_setup/enter/register are denied with
// ENOSYS (errno 38), NOT KILL — the same treatment as clone3, and for the same
// reason. ENOSYS is chosen because modern runtimes feature-probe io_uring (libuv
// >= 1.45 attempts io_uring_setup and falls back to its thread pool on failure), so
// a KILL would risk a fatal SIGSYS on a future toolchain bump, whereas ENOSYS looks
// exactly like a kernel without io_uring (the documented fallback trigger). Because
// io_uring_setup itself is denied, no ring is ever created, which closes both halves
// of the finding: the io_uring kernel surface and the kernel-thread I/O channel.
//
// Unlike the KILL tests above (6-9), the program is NOT killed: it runs to
// completion. So the shape mirrors clone3 in test 16 — io_uring_setup returns
// -1/ENOSYS, the program prints its result and reaches AFTER.
//
// Holds (expected): BEFORE prints; io_uring_setup returns -1 with errno 38
// (IO_URING_DENIED ret=-1 errno=38), so no ring fd; AFTER prints; the run is NOT a
// SIGSYS kill. No "IO_URING_OK" anywhere.
//
// Did NOT hold (ESCAPE): "IO_URING_OK fd=…" prints — io_uring_setup returned a real
// ring fd, so the io_uring machinery is reachable by untrusted code.
func TestSeccompIoUringBlocked(t *testing.T) {
	const src = `
#define _GNU_SOURCE
#include <stdio.h>
#include <string.h>
#include <errno.h>
#include <unistd.h>
#include <sys/syscall.h>

#ifndef __NR_io_uring_setup
#define __NR_io_uring_setup 425
#endif

/* io_uring_setup(unsigned entries, struct io_uring_params *p) returns a ring fd.
   struct io_uring_params is 120 bytes on x86-64; over-size the buffer to 256 so a
   (hypothetical) SUCCESS would not corrupt the stack when the kernel writes the
   sq/cq offsets back — we want a clean IO_URING_OK in the escape case, not a crash
   that masks it. */
int main(void){
    printf("BEFORE\n");
    fflush(stdout);

    unsigned char params[256];
    memset(params, 0, sizeof(params));
    long fd = syscall(__NR_io_uring_setup, 8u, params);
    if (fd >= 0) {
        printf("IO_URING_OK fd=%ld\n", fd);
    } else {
        printf("IO_URING_DENIED ret=%ld errno=%d\n", fd, errno);
    }
    fflush(stdout);

    printf("AFTER\n");
    fflush(stdout);
    return 0;
}
`
	resp := runC(t, src, "")
	build := resp.Build
	run := resp.Tests[0]
	t.Logf("io_uring: build=%s run=%s stdout=%q stderr=%q", build.Status, run.Status, run.Stdout, run.Stderr)

	if build.Status != "ok" {
		t.Fatalf("io_uring: C build did not succeed (status %q, stderr %q) — cannot conclude anything", build.Status, build.Stderr)
	}
	if !strings.Contains(run.Stdout, "BEFORE") {
		t.Errorf("program did not reach the BEFORE marker; stdout=%q stderr=%q", run.Stdout, run.Stderr)
	}
	// The headline escape: io_uring_setup returned a real ring fd.
	if strings.Contains(run.Stdout, "IO_URING_OK") {
		t.Errorf("ESCAPE: io_uring_setup created a ring (Finding B still open) — io_uring is reachable by untrusted code; stdout=%q", run.Stdout)
	}
	// It must have been refused with ENOSYS (errno 38), proving the ENOSYS rule fired
	// rather than io_uring_setup succeeding.
	if !strings.Contains(run.Stdout, "IO_URING_DENIED") || !strings.Contains(run.Stdout, "errno=38") {
		t.Errorf("io_uring_setup was not refused with ENOSYS as expected; stdout=%q", run.Stdout)
	}
	// ENOSYS is a graceful denial, not a kill: the program must run PAST the call to
	// print AFTER (a SIGSYS kill would stop before it), so the run is NOT runtime_error.
	if !strings.Contains(run.Stdout, "AFTER") {
		t.Errorf("program did not reach AFTER — io_uring was denied by a kill rather than the intended ENOSYS; stdout=%q stderr=%q", run.Stdout, run.Stderr)
	}
	if run.Status == "runtime_error" {
		t.Errorf("expected a clean completion (ENOSYS fallback, not a SIGSYS kill), got runtime_error; stdout=%q stderr=%q", run.Stdout, run.Stderr)
	}
}
