//go:build escapetests

package escapetests

import (
	"strings"
	"testing"
)

// The boundary group (tests 14-15) leaves the three named Phase 1 pillars
// (filesystem, seccomp, cgroups) and probes the shared-kernel boundary that
// docs/security.md flags as residual attack surface: the capabilities the
// sandboxed process actually holds, and whether it can reach the network.
//
// Both targets are open questions in the threat model. The container runs
// --privileged (a very broad capability grant) and the threat model lists
// "--privileged" and "network namespace configuration needs review" as known
// limitations. These two tests answer them against the live sandbox rather than
// by assumption: --privileged grants capabilities to the CONTAINER, but nsjail
// can drop them for the CHILD independently, and nsjail clones a fresh network
// namespace by default unless told otherwise (the same kind of tool-default
// isolation Group 1's test 5 found for the PID namespace).

// Test 14 — effective capabilities inside the sandbox.
//
// Attempts: read /proc/self/status from a sandboxed process and inspect the five
// capability masks (CapInh, CapPrm, CapEff, CapBnd, CapAmb), then corroborate with
// two capability-gated syscalls from C (chroot needs CAP_SYS_CHROOT, sethostname
// needs CAP_SYS_ADMIN).
//
// The question this answers: the container runs --privileged, so the CONTAINER has
// a near-full capability set. Does the sandboxed CHILD inherit that, or does nsjail
// drop capabilities for it regardless? These can differ — nsjail drops all
// capabilities for the child by default (it does not pass --keep_caps), so even a
// --privileged container yields a capability-less sandbox.
//
// Holds (expected, and the GOOD-NEWS finding): every mask is
// 0000000000000000 — including CapBnd, the bounding set, whose being empty means
// the process can never REGAIN any capability even by exec'ing a setuid-root binary.
// The process runs as uid 0 but is capability-less, so the gated syscalls fail with
// EPERM (errno 1) rather than succeeding — and they are NOT seccomp-killed (chroot
// and sethostname are not on the deny-list), so the program runs to completion and
// the EPERM is a true capability check, not a SIGSYS.
//
// Did NOT hold: any mask is non-zero (the child inherited container capabilities),
// or a gated syscall SUCCEEDS — either would mean --privileged's broad grant reaches
// inside the sandbox, a materially larger attack surface than the threat model wants.
func TestEffectiveCapabilities(t *testing.T) {
	// Primary evidence: the kernel's own view of the process's capability masks.
	const statusSrc = `
with open('/proc/self/status') as f:
    for line in f:
        if line[:3] == 'Cap' or line[:4] == 'Uid:' or line.startswith('NoNewPrivs') or line.startswith('Seccomp:'):
            print(line.rstrip(), flush=True)
`
	run := runPy3(t, statusSrc)
	t.Logf("/proc/self/status (caps): status=%s stdout=%q stderr=%q", run.Status, run.Stdout, run.Stderr)

	caps := []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"}
	for _, name := range caps {
		v := capValue(run.Stdout, name)
		if v == "" {
			t.Errorf("%s: not present in /proc/self/status — cannot verify it is empty; stdout=%q", name, run.Stdout)
			continue
		}
		// A capability mask of all zeros means no capabilities in that set. CapBnd
		// (the bounding set) being empty is the strongest of the five: it caps what
		// the process could ever acquire, so an empty CapBnd forecloses regaining
		// any capability via setuid binaries.
		if !isAllZeroHex(v) {
			t.Errorf("ESCAPE SURFACE: %s = %q is NON-EMPTY — the sandboxed child holds capabilities "+
				"(likely inherited from the --privileged container); expected all zeros", name, v)
		} else {
			t.Logf("%s = %s (empty — no capabilities in this set)", name, v)
		}
	}
	// The process is uid 0 (root) but, per the masks above, capability-less: root
	// without capabilities. Log it so the "uid 0 yet powerless" shape is explicit.
	if uid := capValue(run.Stdout, "Uid"); uid != "" {
		t.Logf("Uid line: %q (root, but capability-less per the masks above)", uid)
	}

	// Corroboration: two capability-gated syscalls. With an empty CapEff they must
	// fail with EPERM, and because neither is on the seccomp deny-list the program
	// runs PAST them to print AFTER — so an EPERM here is a real capability denial,
	// distinct from a Group-2 SIGSYS kill.
	const gatedSrc = `
#include <stdio.h>
#include <unistd.h>
#include <errno.h>
#include <string.h>
int main(void){
    printf("BEFORE\n"); fflush(stdout);
    int r = chroot("/");
    printf("chroot rc=%d errno=%d(%s)\n", r, errno, strerror(errno)); fflush(stdout);
    char h[] = "pwn";
    int r2 = sethostname(h, 3);
    printf("sethostname rc=%d errno=%d(%s)\n", r2, errno, strerror(errno)); fflush(stdout);
    printf("AFTER\n");
    return 0;
}
`
	resp := runC(t, gatedSrc, "")
	build := resp.Build
	crun := resp.Tests[0]
	t.Logf("capability-gated syscalls: build=%s run=%s stdout=%q stderr=%q", build.Status, crun.Status, crun.Stdout, crun.Stderr)
	if build.Status != "ok" {
		t.Fatalf("capability corroboration: C build did not succeed (status %q, stderr %q)", build.Status, build.Stderr)
	}
	// The program must run to completion (AFTER), proving these are capability checks
	// returning EPERM, not seccomp kills (which would stop before AFTER).
	if !strings.Contains(crun.Stdout, "AFTER") {
		t.Errorf("capability corroboration: program did not reach AFTER — the gated syscalls were not "+
			"cleanly EPERM'd (a kill would stop here); stdout=%q stderr=%q", crun.Stdout, crun.Stderr)
	}
	if strings.Contains(crun.Stdout, "chroot rc=0") {
		t.Errorf("ESCAPE: chroot() SUCCEEDED — the sandbox holds CAP_SYS_CHROOT; stdout=%q", crun.Stdout)
	} else if !strings.Contains(crun.Stdout, "chroot rc=-1 errno=1") {
		t.Errorf("chroot: expected rc=-1 errno=1 (EPERM, no CAP_SYS_CHROOT), got something else; stdout=%q", crun.Stdout)
	}
	if strings.Contains(crun.Stdout, "sethostname rc=0") {
		t.Errorf("ESCAPE: sethostname() SUCCEEDED — the sandbox holds CAP_SYS_ADMIN; stdout=%q", crun.Stdout)
	} else if !strings.Contains(crun.Stdout, "sethostname rc=-1 errno=1") {
		t.Errorf("sethostname: expected rc=-1 errno=1 (EPERM, no CAP_SYS_ADMIN), got something else; stdout=%q", crun.Stdout)
	}
}

// Test 15 — outbound network connection.
//
// Attempts: socket(AF_INET, SOCK_STREAM) + connect() to 8.8.8.8:53 from inside the
// sandbox, using a non-blocking connect + select() with a 5-second cap so the test
// can DISTINGUISH the three possible outcomes rather than just blocking:
//   - fail immediately (no route / network unreachable) → ENETUNREACH at once,
//   - succeed (network reachable — the sandbox is NOT isolated),
//   - hang (routing present but the peer never answers) → the 5s select times out.
// Corroborated by reading /proc/net/dev and /proc/net/route from py3 to show what
// interfaces and routes the sandbox's network namespace actually contains.
//
// This answers docs/security.md's open "network namespace configuration needs review"
// item. nsjail clones a fresh network namespace by default (no --disable_clone_newnet
// is emitted by buildNsjailArgs), so the sandbox gets an empty network stack: only a
// (down) loopback device and no routes.
//
// Holds (expected, GOOD NEWS): connect() fails IMMEDIATELY with ENETUNREACH
// (errno 101) — there is no route to anywhere because the namespace has no external
// interface. The corroboration shows /proc/net/dev lists only "lo" and /proc/net/route
// is empty (not even a default route).
//
// Did NOT hold: connect() succeeds — sandboxed code can reach the internet, an
// exfiltration / SSRF / C2 channel the threat model did not intend.
func TestOutboundNetworkBlocked(t *testing.T) {
	const src = `
#include <stdio.h>
#include <string.h>
#include <errno.h>
#include <unistd.h>
#include <fcntl.h>
#include <time.h>
#include <sys/socket.h>
#include <sys/select.h>
#include <netinet/in.h>
#include <arpa/inet.h>
static double now(){struct timespec t;clock_gettime(CLOCK_MONOTONIC,&t);return t.tv_sec+t.tv_nsec/1e9;}
int main(void){
    printf("BEFORE\n"); fflush(stdout);
    int fd = socket(AF_INET, SOCK_STREAM, 0);
    if (fd < 0){ printf("SOCKET_FAILED errno=%d(%s)\n", errno, strerror(errno)); fflush(stdout); return 0; }
    int fl = fcntl(fd, F_GETFL, 0); fcntl(fd, F_SETFL, fl | O_NONBLOCK);
    struct sockaddr_in a; memset(&a,0,sizeof a);
    a.sin_family = AF_INET; a.sin_port = htons(53); inet_pton(AF_INET, "8.8.8.8", &a.sin_addr);
    double t0 = now();
    int r = connect(fd, (struct sockaddr*)&a, sizeof a);
    if (r == 0){ printf("CONNECT_IMMEDIATE_OK elapsed=%.3f\n", now()-t0); fflush(stdout); return 0; }
    if (errno != EINPROGRESS){ printf("CONNECT_FAILED_IMMEDIATE errno=%d(%s) elapsed=%.3f\n", errno, strerror(errno), now()-t0); fflush(stdout); return 0; }
    fd_set w; FD_ZERO(&w); FD_SET(fd,&w);
    struct timeval tv; tv.tv_sec = 5; tv.tv_usec = 0;
    int s = select(fd+1, NULL, &w, NULL, &tv);
    double el = now()-t0;
    if (s == 0){ printf("CONNECT_TIMEOUT_HANG elapsed=%.3f\n", el); fflush(stdout); return 0; }
    int err = 0; socklen_t l = sizeof err; getsockopt(fd, SOL_SOCKET, SO_ERROR, &err, &l);
    if (err == 0) printf("CONNECT_OK_ASYNC elapsed=%.3f\n", el);
    else printf("CONNECT_FAILED_ASYNC err=%d(%s) elapsed=%.3f\n", err, strerror(err), el);
    fflush(stdout);
    return 0;
}
`
	resp := runC(t, src, "")
	build := resp.Build
	run := resp.Tests[0]
	t.Logf("outbound connect: build=%s run=%s dur=%dms stdout=%q stderr=%q", build.Status, run.Status, run.DurationMs, run.Stdout, run.Stderr)
	if build.Status != "ok" {
		t.Fatalf("network test: C build did not succeed (status %q, stderr %q)", build.Status, build.Stderr)
	}
	if !strings.Contains(run.Stdout, "BEFORE") {
		t.Fatalf("network test: program did not start (no BEFORE marker); stdout=%q stderr=%q", run.Stdout, run.Stderr)
	}
	// The decisive assertion: a successful connect is a real escape.
	if strings.Contains(run.Stdout, "CONNECT_IMMEDIATE_OK") || strings.Contains(run.Stdout, "CONNECT_OK_ASYNC") {
		t.Fatalf("ESCAPE: outbound connect to 8.8.8.8:53 SUCCEEDED — sandboxed code can reach the network "+
			"(exfiltration / C2 channel); stdout=%q", run.Stdout)
	}
	switch {
	case strings.Contains(run.Stdout, "CONNECT_FAILED_IMMEDIATE"):
		// expected: ENETUNREACH at once — the netns has no route to anywhere.
		if !strings.Contains(run.Stdout, "errno=101") {
			t.Logf("network blocked, but not with ENETUNREACH(101) — a different immediate failure; stdout=%q", run.Stdout)
		} else {
			t.Logf("HELD: connect failed immediately with ENETUNREACH(101) — isolated network namespace, no route out")
		}
	case strings.Contains(run.Stdout, "CONNECT_TIMEOUT_HANG"):
		// Also "blocked" (no data leaves), but a different shape: routing exists yet
		// the peer is unreachable. Worth distinguishing from clean namespace isolation.
		t.Logf("network blocked by HANG (select timed out) rather than immediate ENETUNREACH — routing may be "+
			"present but unreachable; stdout=%q", run.Stdout)
	case strings.Contains(run.Stdout, "CONNECT_FAILED_ASYNC"):
		t.Logf("connect failed asynchronously (not an immediate reject); stdout=%q", run.Stdout)
	default:
		t.Errorf("network test: unexpected outcome — no recognized connect result; stdout=%q stderr=%q", run.Stdout, run.Stderr)
	}

	// Corroboration: what does the sandbox's network namespace actually contain?
	const ifSrc = `
import os
try:
    with open('/proc/net/dev') as f:
        ifaces = []
        for line in f.read().splitlines()[2:]:
            name = line.split(':', 1)[0].strip()
            if name:
                ifaces.append(name)
        print('IFACES', ifaces, flush=True)
except Exception as e:
    print('IFACES_ERR', e, flush=True)
try:
    with open('/proc/net/route') as f:
        routes = f.read().splitlines()[1:]
        print('ROUTE_ENTRIES', len([r for r in routes if r.strip()]), flush=True)
except Exception as e:
    print('ROUTE_ERR', e, flush=True)
`
	ifr := runPy3(t, ifSrc)
	t.Logf("netns contents: status=%s stdout=%q", ifr.Status, ifr.Stdout)
	// The only interface should be loopback; no external NIC means no way out.
	if strings.Contains(ifr.Stdout, "eth0") {
		t.Errorf("network namespace contains an external interface (eth0) — the sandbox is NOT on an isolated "+
			"empty netns; stdout=%q", ifr.Stdout)
	}
	if !strings.Contains(ifr.Stdout, "ROUTE_ENTRIES 0") {
		t.Logf("note: routing table is non-empty (expected 0 entries for a fresh netns); stdout=%q", ifr.Stdout)
	} else {
		t.Logf("HELD: only loopback present and routing table empty — fresh, isolated network namespace")
	}
}

// capValue returns the value (tab/space-trimmed remainder) of the first line in
// status whose label (text before the first ':') equals name, or "" if absent.
// Used to pull individual fields out of a /proc/self/status dump.
func capValue(status, name string) string {
	for _, line := range strings.Split(status, "\n") {
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		if line[:i] == name {
			return strings.TrimSpace(line[i+1:])
		}
	}
	return ""
}

// isAllZeroHex reports whether s is non-empty and every character is '0' — i.e. a
// capability mask with no bits set (no capabilities). A 16-char "0000000000000000"
// is the empty 64-bit capability set.
func isAllZeroHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}
