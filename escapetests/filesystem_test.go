//go:build escapetests

package escapetests

import (
	"strings"
	"testing"
)

// The filesystem-isolation group (tests 1-5) targets the first Phase 1 pillar:
// a fresh mount namespace per request with a minimal, explicitly bind-mounted
// read-only root plus a single writable per-request work directory. Each test
// submits a py3 program that tries to reach something OUTSIDE that minimal set
// and asserts the sandbox denied it.

// Test 1 — Read /etc/passwd.
//
// Attempts: open("/etc/passwd") from inside the sandbox.
//
// Holds (expected): the open fails. /etc is not in py3's bind-mount profile
// (only the interpreter, its shared libraries and the Python stdlib are), so
// inside the mount namespace the path simply does not exist — a
// FileNotFoundError, not a permission error.
//
// Did NOT hold: the program prints "OPENED" followed by real passwd content,
// meaning the host /etc leaked into the sandbox.
func TestReadEtcPasswd(t *testing.T) {
	const src = `
try:
    with open('/etc/passwd') as f:
        data = f.read()
    print('OPENED', len(data))
    print(data[:200])
except Exception as e:
    print('DENIED', type(e).__name__, e)
`
	res := runPy3(t, src)
	t.Logf("status=%s stdout=%q stderr=%q", res.Status, res.Stdout, res.Stderr)

	if strings.Contains(res.Stdout, "OPENED") {
		t.Errorf("ESCAPE: /etc/passwd was readable inside the sandbox; stdout=%q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "DENIED") {
		t.Errorf("unexpected output — neither OPENED nor DENIED seen; stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
	// Filesystem isolation works by absence, not by permission: the file should
	// not exist in the mount namespace at all.
	if !strings.Contains(res.Stdout, "FileNotFoundError") {
		t.Logf("note: denied, but not via FileNotFoundError — reason was: %q", res.Stdout)
	}
}

// Test 2 — Read paths outside the bind-mounted set.
//
// Attempts: open() / listdir() on host paths that are deliberately NOT in any
// py3 mount profile — /root (host root's home), /var/log (host logs), /home,
// and /proc/1 (which the notes flag as "the host's init process info").
//
// Holds (expected): /root, /var/log and /home are absent from the mount
// namespace, so each access fails with FileNotFoundError.
//
// The /proc/1 case turned out subtler than the notes assumed and is the
// valuable finding here: /proc IS mounted in the sandbox, and /proc/1 IS
// readable — but it is NOT the host's init. nsjail creates a fresh PID
// namespace (its default) and mounts a procfs scoped to it, so pid 1 inside the
// jail is the sandboxed python3 process ITSELF (Name: python3, PPid: 0), not
// the host's init/goboxd. Reading your own /proc entry is not a leak. The test
// therefore asserts /proc/1 reflects the sandboxed interpreter, which is
// positive evidence of PID-namespace isolation — examined directly in test 5.
//
// Did NOT hold: /root, /var/log or /home returns real host content, OR
// /proc/1/status names a foreign host process (e.g. systemd/init/goboxd),
// which would mean the host PID view leaked in.
func TestReadOutsideBindMounts(t *testing.T) {
	const src = `
import os
for p in ('/root', '/var/log', '/home', '/proc/1/status'):
    try:
        if os.path.isdir(p):
            entries = os.listdir(p)
            print('LISTED', p, len(entries), entries[:10])
        else:
            with open(p) as f:
                print('READ', p, repr(f.read(120)))
    except Exception as e:
        print('DENIED', p, type(e).__name__)
`
	res := runPy3(t, src)
	t.Logf("status=%s stdout=%q stderr=%q", res.Status, res.Stdout, res.Stderr)

	// Host paths must be absent from the mount namespace entirely.
	for _, p := range []string{"/root", "/var/log", "/home"} {
		if strings.Contains(res.Stdout, "LISTED "+p) || strings.Contains(res.Stdout, "READ "+p) {
			t.Errorf("ESCAPE: %s was accessible inside the sandbox; stdout=%q", p, res.Stdout)
		}
		if !strings.Contains(res.Stdout, "DENIED "+p) {
			t.Errorf("no DENIED line for %s — unexpected; stdout=%q", p, res.Stdout)
		}
	}

	// /proc/1 is reachable, but PID-namespace isolation means it is the
	// sandboxed process itself, not the host init. Confirm it reads as our own
	// python3 interpreter and exposes no foreign host process.
	if !strings.Contains(res.Stdout, "READ /proc/1/status") {
		t.Errorf("/proc/1/status was not readable; expected the sandbox's own pid-1 entry; stdout=%q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "Name:\\tpython3") {
		t.Errorf("ESCAPE/LEAK: /proc/1 is not the sandboxed python3 — host PID view may have leaked; stdout=%q", res.Stdout)
	}
	for _, foreign := range []string{"systemd", "/sbin/init", "goboxd"} {
		if strings.Contains(res.Stdout, foreign) {
			t.Errorf("ESCAPE/LEAK: /proc/1 names a host process %q; stdout=%q", foreign, res.Stdout)
		}
	}
}

// Test 3 — Write outside the per-request work directory.
//
// Attempts: create files at the tmpfs root (/escape), at /tmp/escape, and
// inside a read-only mounted area (/usr/escape). As a control, also write
// inside the current working directory (the per-request work dir), which SHOULD
// succeed — that is the one writable, persistent path the sandbox is given.
//
// Holds (expected): every write outside the work dir fails. The py3 run profile
// mounts everything read-only and gives the run step NO writable /tmp at all
// (only build steps get a tmpfs /tmp), so /tmp does not even exist; read-only
// bind mounts reject writes; and the tmpfs root, if writable, is ephemeral and
// not the host filesystem. The control write to the work dir succeeds.
//
// Did NOT hold: a write outside the work dir succeeds AND lands on a path that
// outlives the request or is shared with the host / other languages.
func TestWriteOutsideWorkDir(t *testing.T) {
	const src = `
import os
targets = ['/escape.txt', '/tmp/escape.txt', '/usr/escape.txt', '/etc/escape.txt']
for p in targets:
    try:
        with open(p, 'w') as f:
            f.write('x')
        print('WROTE', p)
    except Exception as e:
        print('DENIED', p, type(e).__name__)

# Control: the per-request work directory (cwd) must be writable.
try:
    with open('control.txt', 'w') as f:
        f.write('ok')
    print('WROTE_CWD', os.getcwd())
except Exception as e:
    print('CWD_DENIED', type(e).__name__, e)
`
	res := runPy3(t, src)
	t.Logf("status=%s stdout=%q stderr=%q", res.Status, res.Stdout, res.Stderr)

	for _, p := range []string{"/escape.txt", "/tmp/escape.txt", "/usr/escape.txt", "/etc/escape.txt"} {
		if strings.Contains(res.Stdout, "WROTE "+p) {
			t.Errorf("ESCAPE: write succeeded outside the work dir at %s; stdout=%q", p, res.Stdout)
		}
		if !strings.Contains(res.Stdout, "DENIED "+p) {
			t.Errorf("no DENIED line for %s — unexpected; stdout=%q", p, res.Stdout)
		}
	}
	// The control write to the work dir must succeed, or the sandbox would be
	// unusable (programs need somewhere to write).
	if !strings.Contains(res.Stdout, "WROTE_CWD") {
		t.Errorf("control write to the work directory failed; the sandbox should allow it; stdout=%q", res.Stdout)
	}
}

// Test 4 — List the root directory (/).
//
// Attempts: os.listdir('/') to enumerate exactly what the sandbox's minimal
// filesystem contains. This is a sanity check, not an attack: a populated /
// here is not itself an "escape", but it documents the real attack surface and
// confirms only the expected minimal set is present.
//
// Holds (expected): / contains only what py3's mount profile bind-mounted plus
// nsjail's own scaffolding — the dirs needed to mount the interpreter, its libs
// and stdlib (e.g. usr, lib, lib64), the procfs nsjail mounts, and the entries
// on the path to the work dir (tmp) — and notably NONE of the host's
// distinctive top-level dirs (root, home, var, etc, opt, srv, boot, the goboxd
// /app dir, the Docker socket, …).
//
// Did NOT hold: host-distinctive entries appear at /, meaning the host root
// filesystem (not a minimal constructed root) is visible.
func TestListRootDirectory(t *testing.T) {
	const src = `
import os
print('ROOT', sorted(os.listdir('/')))
`
	res := runPy3(t, src)
	t.Logf("status=%s stdout=%q stderr=%q", res.Status, res.Stdout, res.Stderr)

	if !strings.Contains(res.Stdout, "ROOT ") {
		t.Fatalf("could not list /; stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}

	// None of these host-distinctive top-level entries should be visible. Their
	// presence would mean the host root leaked in rather than a minimal mount
	// namespace being constructed. (/app is the goboxd working dir in the image;
	// the Docker socket would be a particularly serious leak.)
	hostMarkers := []string{
		"'root'", "'home'", "'var'", "'etc'", "'opt'", "'srv'",
		"'boot'", "'media'", "'mnt'", "'app'", "'docker.sock'",
	}
	for _, m := range hostMarkers {
		if strings.Contains(res.Stdout, m) {
			t.Errorf("ESCAPE: host-distinctive entry %s visible at /; the host root may have leaked; stdout=%q", m, res.Stdout)
		}
	}
}

// Test 5 — Access another process's /proc/<pid>/{environ,maps} (PID namespace).
//
// Attempts: enumerate every numeric pid under /proc, report this process's own
// pid, and try to read /proc/1/environ, /proc/1/maps and a few other pids'
// environ — the classic way to steal another process's secrets/memory layout.
//
// What Phase 1 actually configured: nothing explicit. The nsjail arg builder
// (internal/runner/nsjail.go) never passes any --*_clone_newpid flag, so PID
// isolation rests entirely on nsjail's DEFAULT of cloning a fresh PID namespace
// for the child. docs/security.md's threat model lists mount/seccomp/cgroups as
// the three pillars but does not claim or verify PID-namespace isolation. This
// test is what verifies it empirically — new information beyond Phase 1.
//
// Holds (expected): this process is pid 1 (it is the init of a fresh PID
// namespace), and the ONLY numeric entry under /proc is 1 — itself. No other
// process (the host's goboxd, the nsjail parent, the host init) is visible, so
// there is no foreign environ/maps to read. /proc/1/{environ,maps} are readable
// but they are this process's OWN — and environ holds only the PATH nsjail
// injects, no host secrets.
//
// Did NOT hold: numeric pids other than 1 appear under /proc, or this process
// is not pid 1 — either would mean the host PID view is (partly) visible and a
// foreign process's environ/maps could be read.
func TestProcPidNamespaceIsolation(t *testing.T) {
	const src = `
import os
pids = sorted(int(n) for n in os.listdir('/proc') if n.isdigit())
print('MYPID', os.getpid())
print('PIDS', pids)

# Reading our OWN environ is fine; assert it carries no host secrets (nsjail
# starts the child with an empty env plus only PATH).
try:
    with open('/proc/1/environ', 'rb') as f:
        env = f.read()
    print('ENVIRON', repr(env))
except Exception as e:
    print('ENVIRON_DENIED', type(e).__name__)

# Try to reach OTHER processes' memory/secrets. In a fresh PID namespace there
# are none, so every one of these should fail.
for pid in (2, 100, 1000):
    for kind in ('environ', 'maps'):
        p = '/proc/%d/%s' % (pid, kind)
        try:
            with open(p, 'rb') as f:
                f.read()
            print('READ_OTHER', p)
        except Exception as e:
            print('NO_OTHER', p, type(e).__name__)
`
	res := runPy3(t, src)
	t.Logf("status=%s stdout=%q stderr=%q", res.Status, res.Stdout, res.Stderr)

	// New PID namespace: we are pid 1 and the only pid visible is 1.
	if !strings.Contains(res.Stdout, "MYPID 1\n") {
		t.Errorf("expected to be pid 1 in a fresh PID namespace; stdout=%q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "PIDS [1]\n") {
		t.Errorf("ESCAPE: more than just our own pid is visible under /proc — host PID view may have leaked; stdout=%q", res.Stdout)
	}

	// No foreign process should be readable at all.
	if strings.Contains(res.Stdout, "READ_OTHER") {
		t.Errorf("ESCAPE: read another process's /proc entry; stdout=%q", res.Stdout)
	}

	// Our own environ must not carry host secrets — only the injected PATH.
	if strings.Contains(res.Stdout, "ENVIRON ") {
		if !strings.Contains(res.Stdout, "PATH=") {
			t.Logf("note: /proc/1/environ did not contain PATH — unexpected but not a leak; stdout=%q", res.Stdout)
		}
		for _, secret := range []string{"AWS", "TOKEN", "SECRET", "KEY=", "PASSWORD"} {
			if strings.Contains(res.Stdout, secret) {
				t.Errorf("possible secret in /proc/1/environ (%q); stdout=%q", secret, res.Stdout)
			}
		}
	}
}

// Test 19 — single-file disk write is bounded; bulk disk-fill is a documented
// limitation (audit Finding D).
//
// The per-request work dir is a host-backed bind mount (os.MkdirTemp under the
// container's /tmp, bind-mounted writable). It MUST be host-backed because build and
// run are separate nsjail invocations that share state through it — the compiler
// writes the artifact in the build step and the run step executes it — so it cannot
// be swapped for a size-limited tmpfs without re-architecting the build/run split.
// And a true per-request disk quota (loop-mounted fs / project quotas) is
// disproportionate here. So Finding D's disk-fill DoS is, in the general (many-file)
// case, a DOCUMENTED KNOWN LIMITATION (see docs/security-audit-findings.md), with the
// recommended operational mitigation of sizing/monitoring the container's /tmp.
//
// What IS already enforced, and what this test verifies, is the SINGLE-FILE bound:
// nsjail's default rlimit_fsize (1 MiB) caps the size of any one file, so a program
// cannot write an arbitrarily large single file — the write past 1 MiB raises EFBIG
// or the process is SIGXFSZ-terminated, either way bounding the file. The test writes
// to ONE growing file and asserts it never reaches an obviously-large size, and that
// the service survives. It deliberately does NOT exercise the many-small-files path
// (that would actually fill disk); that residual is the documented limitation.
//
// Holds (expected): the program is cut off well before writing a large file (no
// "MiB=64" etc.), and a trivial run afterwards still succeeds.
//
// Did NOT hold: the program reports writing a very large single file (rlimit_fsize
// is not in force), or the service does not survive.
func TestSingleFileWriteBounded(t *testing.T) {
	// Append 256 KiB chunks to a single file in the work dir, flushing to the kernel
	// each time, and report cumulative size. rlimit_fsize (1 MiB default) should stop
	// it near 1 MiB — either via OSError(EFBIG) caught here, or by SIGXFSZ terminating
	// the process (Python installs no handler), which surfaces as runtime_error. Either
	// way the single file is bounded.
	const src = `
import os
chunk = b'x' * (256 * 1024)
total = 0
try:
    with open('big.bin', 'wb') as f:
        for _ in range(8192):  # would be 2 GiB if unbounded
            f.write(chunk)
            f.flush()
            os.fsync(f.fileno())
            total += len(chunk)
            if total % (1024 * 1024) == 0:
                print('MiB=%d' % (total // (1024 * 1024)), flush=True)
    print('DONE total_bytes=%d' % total, flush=True)
except Exception as e:
    print('WRITE_ERR after_bytes=%d %s' % (total, type(e).__name__), flush=True)
`
	res := runPy3(t, src)
	t.Logf("single-file write: status=%s stdout=%q stderr=%q", res.Status, lastLines(res.Stdout, 3), res.Stderr)

	// The decisive assertion: a single file must NOT grow large. With rlimit_fsize at
	// 1 MiB the program is cut off near 1 MiB, so anything from ~64 MiB up means the
	// per-file cap is not in force — the disk-fill exposure would then be far worse
	// than the documented (file-count-only) residual.
	for _, big := range []string{"MiB=64", "MiB=128", "MiB=256", "MiB=512", "MiB=1024"} {
		if strings.Contains(res.Stdout, big) {
			t.Errorf("single-file write reached %s — rlimit_fsize is not bounding single files; stdout-tail=%q", big, lastLines(res.Stdout, 3))
		}
	}
	if strings.Contains(res.Stdout, "DONE total_bytes=") {
		// Completing the full 2 GiB loop would mean no per-file bound at all.
		t.Errorf("the write loop completed — a single file was not bounded by rlimit_fsize; stdout-tail=%q", lastLines(res.Stdout, 3))
	}

	// Containment: the service survives the write attempt.
	after := runPy3(t, "print('alive')")
	if !strings.Contains(after.Stdout, "alive") {
		t.Errorf("CONTAINMENT FAILURE: service did not survive the disk-write attempt (status %q, stdout %q)", after.Status, after.Stdout)
	} else {
		t.Logf("containment OK: service responsive after the disk-write attempt")
	}
}

// Test 20 — /proc leaks host facts: a DOCUMENTED KNOWN LIMITATION (audit Finding F).
//
// /proc is mounted (read-only) in the sandbox, and several files there are NOT
// namespaced, so they reflect the HOST: /proc/cpuinfo (CPU model/cores),
// /proc/meminfo (total/free RAM), /proc/loadavg (host load — a low-bandwidth
// co-tenant side channel) and /proc/version (kernel version, useful for picking a
// kernel exploit). This is an information-disclosure / fingerprinting issue, not an
// isolation breach: nothing here lets sandboxed code read, write or affect anything
// outside its sandbox.
//
// It is documented rather than fixed because masking these files cannot be done
// reliably without risking runtime breakage: the JVM and V8/Node read /proc/cpuinfo
// (and the JVM may consult /proc/meminfo) for ergonomic sizing of thread pools and
// heaps, so feeding them a fake or empty file risks mis-sizing or startup failure
// across the seven runtimes. The clean fix is a stronger sandbox that synthesises
// /proc (gVisor) — see docs/security.md "Strengthening the boundary". See
// docs/security-audit-findings.md Finding F for the full per-file reasoning.
//
// This test is therefore like test 12 (the zero-page boundary): it DOCUMENTS the
// current behaviour rather than asserting a fix. It reads the four files and logs
// what leaks; it does NOT fail on the leak (that is the known, accepted state). It
// only fails if the behaviour changes in a way worth noticing — e.g. /proc is no
// longer mounted at all (which would break tests 2 and 5 that rely on it) — so that a
// future change to /proc handling does not pass silently.
func TestProcHostInfoLeak(t *testing.T) {
	const src = `
def first_line(p):
    try:
        with open(p) as f:
            return f.readline().strip()
    except Exception as e:
        return 'ERR ' + type(e).__name__

def field(p, key):
    try:
        with open(p) as f:
            for line in f:
                if line.startswith(key):
                    return line.strip()
    except Exception as e:
        return 'ERR ' + type(e).__name__
    return 'ABSENT'

print('CPUINFO', field('/proc/cpuinfo', 'model name'), flush=True)
print('MEMINFO', field('/proc/meminfo', 'MemTotal'), flush=True)
print('LOADAVG', first_line('/proc/loadavg'), flush=True)
print('VERSION', first_line('/proc/version'), flush=True)
`
	res := runPy3(t, src)
	t.Logf("proc leak: status=%s\n%s", res.Status, res.Stdout)

	// /proc must still be mounted (tests 2 and 5 depend on it); a wholesale ERR on
	// every file would mean /proc handling changed and those tests' premises shifted.
	if strings.Contains(res.Stdout, "CPUINFO ERR") && strings.Contains(res.Stdout, "VERSION ERR") {
		t.Errorf("/proc appears unmounted (every probe errored) — this changes the premise of tests 2 and 5; stdout=%q", res.Stdout)
	}

	// Document (do not fail) which host facts are visible. These leaks are the known
	// limitation; logging them keeps the current state on the record.
	for _, label := range []string{"CPUINFO model name", "MEMINFO MemTotal", "LOADAVG", "VERSION Linux"} {
		if strings.Contains(res.Stdout, label) {
			t.Logf("KNOWN LIMITATION (Finding F): host fact visible via /proc — %q", label)
		}
	}
}
