# Tracebox — Escape Test Suite (Phase 3)

Runnable, `go test`-based escape attempts that try to break each Phase 1
isolation pillar against the **live, sandboxed container** (Docker + nsjail).
Each test submits a small program through the real `/run` HTTP API and asserts
what the sandbox actually did with it.

## How to run

```bash
docker build -t tracebox .
docker run --privileged --cgroupns=host --rm -d -p 8080:8080 \
    -e GOBOXD_RUNNER=nsjail tracebox

# the escape suite (black-box HTTP client against the live container)
go test -tags escapetests -v ./escapetests/...
```

The target URL defaults to `http://127.0.0.1:8080`; override with
`GOBOXD_ESCAPE_URL`. The suite is tagged `escapetests` so it is excluded from
the normal `go test ./...` run, exactly like the `integration`-tagged tests.

## Outcome legend

- **Held (expected)** — blocked the way Phase 1's design intended.
- **Held (unexpected reason)** — blocked, but not for the predicted reason.
- **Did NOT hold** — the attack succeeded; a real gap.

---

## Group 1 — Filesystem isolation (mount namespace + bind mounts)

Target: each request runs in a fresh mount namespace with a minimal read-only
root (interpreter + libs + stdlib) plus one writable per-request work dir. All
five attempts use `py3`. All five **held**.

| # | Test | Attempt | Result | Outcome |
|---|------|---------|--------|---------|
| 1 | `TestReadEtcPasswd` | `open('/etc/passwd')` | `FileNotFoundError` — `/etc` not in the mount namespace | **Held (expected)** |
| 2 | `TestReadOutsideBindMounts` | read `/root`, `/var/log`, `/home`, `/proc/1` | host dirs all `FileNotFoundError`; `/proc/1` readable but is the sandbox's OWN pid-1 (`Name: python3`, `PPid: 0`), not host init | **Held (expected)** |
| 3 | `TestWriteOutsideWorkDir` | write `/`, `/tmp`, `/usr`, `/etc` | all denied (`OSError` read-only / `FileNotFoundError`); control write to the work dir (`/tmp/goboxd-*`) succeeds | **Held (expected)** |
| 4 | `TestListRootDirectory` | `os.listdir('/')` | `['lib', 'lib64', 'proc', 'tmp', 'usr']` — minimal constructed root, no host top-level dirs | **Held (expected)** |
| 5 | `TestProcPidNamespaceIsolation` | enumerate `/proc` pids; read other pids' `environ`/`maps` | `getpid()==1`; only pid visible is `1` (self); pids 2/100/1000 absent; own environ holds only injected `PATH` | **Held (expected)** |

### Notes on individual tests

**Test 2 — the `/proc/1` subtlety.** The notes anticipated `/proc/1` as "the
host's init process info". It is not. `/proc` *is* mounted in the sandbox and
`/proc/1` *is* readable, but PID-namespace isolation (see test 5) means pid 1
inside the jail is the sandboxed `python3` process itself, not the host init or
`goboxd`. Reading your own `/proc` entry is not a leak, so this is a held
(expected) outcome once the premise is corrected.

**Test 3 — `/tmp` boundary.** `/tmp` itself is a read-only tmpfs root with no
writable space; only the specific per-request work dir (`/tmp/goboxd-<id>`,
bind-mounted writable) accepts writes. The run profile gets no writable `/tmp`
at all — only build steps do. So `/tmp/escape.txt` fails with `OSError` while
the work dir succeeds.

**Test 5 — PID namespace: new information beyond Phase 1.** This is the
notable finding of the group. Phase 1's nsjail argument builder
(`internal/runner/nsjail.go`) passes **no** `--*_clone_newpid` flag, and
`docs/security.md`'s threat model names only mount/seccomp/cgroups as the three
pillars — it never claims or verifies PID-namespace isolation. This test shows
the PID namespace is nonetheless active, inherited from **nsjail's default**
(it clones a fresh PID namespace unless told otherwise): the sandboxed process
is pid 1, the only pid under `/proc` is itself, and no host process
(`goboxd`, the nsjail parent, host init) is visible — so there is no foreign
`/proc/<pid>/environ` or `/maps` to harvest. This is positive isolation that
was implicit (a dependency on a tool default) rather than explicitly configured
or documented in Phase 1; it is now verified. Worth recording in
`docs/security.md` that PID-namespace isolation relies on nsjail's default and
would silently regress if `--disable_clone_newpid` were ever added.

---

## Group 2 — seccomp syscall filtering (the kafel deny-list)

Target: `configs/seccomp.policy`, a kafel deny-list compiled to seccomp-BPF and
applied uniformly to every language via nsjail's `--seccomp_policy`. It KILLs
(`SECCOMP_RET_KILL` → SIGSYS) a fixed set of escape/host-tampering syscalls and
ALLOWs everything else. A SIGSYS kill terminates the process the instant it makes
the call, which nsjail surfaces as a non-zero exit, so the API reports the run as
`runtime_error`.

These tests use **C** — the only one of the seven runtimes that can issue a raw
syscall directly (see the test-8 note). Each program prints a flushed `BEFORE`
marker, issues the denied syscall, then prints `AFTER`; a blocked attempt shows
`BEFORE` but never `AFTER`. All **held**. (Test 16 was added later, when the
red-team audit's Finding A closed the `clone`/`clone3` user-namespace gap; it is
grouped here because it targets the same kafel deny-list.)

| # | Test | Attempt | Result | Outcome |
|---|------|---------|--------|---------|
| 6 | `TestSeccompPtrace` | `ptrace(PTRACE_TRACEME)` | SIGSYS-killed at the call → `runtime_error`; stdout `"BEFORE\n"`, no `AFTER` | **Held (expected)** |
| 7 | `TestSeccompUnshareNewuser` | `unshare(CLONE_NEWUSER)` | SIGSYS-killed → `runtime_error`; `"BEFORE\n"`, no `AFTER` | **Held (expected)** |
| 8 | `TestSeccompUmount` | `umount2(getcwd())` on the work-dir bind mount | SIGSYS-killed → `runtime_error`; `"BEFORE cwd=/tmp/goboxd-…\n"`, no `AFTER` | **Held (expected)** |
| 9 | `TestSeccompSetns` | `open(/proc/self/ns/mnt)` then `setns(fd,0)` | open succeeds (`fd=3`); `setns` SIGSYS-killed → `runtime_error`; `"BEFORE fd=3\n"`, no `AFTER` | **Held (expected)** |
| 10 | `TestSeccompForkAllowed` | `fork()` + `waitpid()` (negative control) | child exits 7, parent prints `FORK_OK`, exit 0 → `accepted` | **Held (expected)** |
| 16 | `TestSeccompCloneNewuserBlocked` | `clone3(CLONE_NEWUSER)` then `clone(CLONE_NEWUSER)` (audit Finding A) | `clone3` → `-1`/`ENOSYS` (`CLONE3_RET=-1 errno=38`, no namespace); `clone` SIGSYS-killed → `runtime_error`; `"BEFORE\nCLONE3_RET=-1 errno=38\n"`, no `CLONE_USERNS_OK`, no `AFTER` | **Held (expected) — Finding A closed** |

### Notes on individual tests

**Test 8 — `mount`/`umount2` are unreachable from the interpreted languages, not
just blocked.** The notes allowed "whatever language is simplest" for this test
rather than C. It still uses C, and that is itself the finding: none of the
high-level runtimes can reach the mount syscalls at all. py3's `import ctypes`
fails (`libffi.so.8: cannot open shared object file` — libffi is a dependency of
`_ctypes.so`, not of `python3`, so `ldd python3` never names it and it is absent
from py3's mount profile), and Python's `os` module has no `mount()`; bash has no
`mount` builtin and no `mount` binary is bound into its profile. So the mount
syscalls are already out of reach before seccomp is consulted — the deny-list is
defense-in-depth behind an already-narrow surface, and C is the simplest language
that can even *attempt* the call. The attempt targets the per-request work
directory (resolved at runtime via `getcwd`, e.g. `/tmp/goboxd-…`) because that
path is a genuine bind mount, so `umount2` is killed on a real target rather than
failing earlier on a nonexistent one.

**Test 9 — open is allowed, setns is not.** Opening `/proc/self/ns/mnt` succeeds
(`fd=3`): procfs is mounted and the namespace link is the process's own, so
obtaining the handle is not blocked. Only `setns` — the syscall that would *join*
a namespace — is killed. The deny-list draws the line at the dangerous operation,
not at touching the namespace file.

**Test 10 — the negative control, and what it proves.** `fork`/`clone` is
deliberately left ALLOWed: the compiled-language build steps shell out to
sub-processes, so blocking it would break normal operation. The child runs, exits
7, the parent reaps it and prints `FORK_OK`, and with `expected_stdout` matching
the verdict is `accepted`. This is the evidence that the deny-list is not
accidentally too broad — a syscall that is not dangerous-by-name keeps working.

**Uniform application confirmed.** Tests 6-9 run under the C profile (whose build
step is `gcc` and run step is `./solution`), while Group 1 exercised py3. Both
being filtered the same way is consistent with the policy living in
`buildNsjailArgs`'s shared base args (keyed off no language branch), i.e. applied
to every language and to both build and run steps.

**Test 16 — the `clone`/`clone3` user-namespace gap, found later and now closed.**
The original suite (tests 6-10) only exercised `unshare` for namespace creation,
which matched the Phase 1 policy as written. The later red-team audit
(`docs/security-audit-findings.md`, Finding A) showed `unshare` was not the only
door: `clone(CLONE_NEWUSER)` and `clone3(.flags=CLONE_NEWUSER)` create the same
user namespace and originally regained a **full capability set**
(`CapEff = 000001ffffffffff`, incl. `CAP_SYS_ADMIN`) inside it — falsifying test
14's "capabilities fully dropped" property. The fix extends the deny-list to all
three primitives: `clone` is arg-filtered on `CLONE_NEWUSER`/`CLONE_NEWNS` exactly
like `unshare` (SIGSYS-KILLed), and `clone3` — whose flags hide behind a pointer
seccomp cannot read — is given `ENOSYS`, so glibc falls back to the filtered
`clone` and a direct `clone3(CLONE_NEWUSER)` just fails. Test 16 drives both paths
and asserts neither creates a namespace; test 10 (fork/clone negative control)
still passes, confirming ordinary process/thread creation is untouched.

### No new gaps (with one later exception, now fixed)

At the time Group 2 was first run, the seccomp group surfaced nothing beyond what
Phase 1's policy and `docs/security.md` documented: every named syscall is killed,
the flag-conditional `unshare` rule fires on `CLONE_NEWUSER`, and the one
explicitly-allowed primitive (fork/clone) works. The one gap discovered afterward —
`clone`/`clone3` reaching the same user namespace `unshare` was filtered for — is
the subject of test 16 above and is now closed; the deny-list semantics in
`configs/seccomp.policy` and the threat model again match observed behaviour.

### Regression

- `go test ./...` — pass (cached; unit/feature suites unaffected).
- `go vet ./...` and `go vet -tags escapetests ./escapetests/...` — clean.

---

## Group 3 — cgroup limits (memory.max, and the missing pids.max)

Target: Phase 1's third pillar, the cgroup v2 memory limit applied via nsjail's
`--cgroup_mem_max` with `--cgroup_mem_swap_max 0` (so the limit is hard, not a
soft spill-to-swap). Each language carries a `memory_kb` budget in
`configs/languages.yaml`; the runner writes it to the child's `memory.max`, and a
process whose **resident** footprint exceeds it is OOM-killed (SIGKILL → nsjail
exit 137 → API `memory_exceeded`). Tests 11-12 hold. **Test 13 originally surfaced
a real gap — `max_processes` was unenforced — which has since been fixed; it now
holds, with process count capped via a cgroup v2 `pids.max` limit.**

| # | Test | Attempt | Result | Outcome |
|---|------|---------|--------|---------|
| 11 | `TestMemoryBombResidentPy3` / `…Java` | allocate + touch every page until the budget is crossed (py3 100 MiB, java 512 MiB) | OOM-killed mid-allocation: py3 at ~90 MiB, java at ~450 MiB → `memory_exceeded` | **Held (expected)** |
| 12 | `TestMemoryBombZeroPage` | `mmap.mmap(-1, 1 GiB, MAP_PRIVATE)` then read-only walk (10× py3's budget) | completes `accepted` in ~210 ms — untouched private pages map the shared zero page, never charged to `memory.max` | **Held (documents a boundary)** |
| 13 | `TestForkBombProcessLimit` | bounded fork bomb, children sleep (linear, self-capped at 2000) | `fork()` fails with **`EAGAIN` at `created=63`** (1 parent + 63 children = 64 tasks = c's `pids.max`); program exits non-zero → `runtime_error` in ~12–72 ms; service responsive after | **Held (expected) — `max_processes` now enforced** |

### Notes on individual tests

**Test 11 — the JVM is the interesting case.** py3's resident bomb is
unremarkable: touch 10 MiB chunks, die at ~90 MiB. java matters because Phase 1
showed `--rlimit_as` could never contain the JVM (it reserves an enormous
*virtual* address space up front), so only a *resident*-memory cgroup limit can
hold it. It does: the JVM is OOM-killed at ~450 MiB of its 512 MiB budget and the
API reports `memory_exceeded` — not a JVM-internal `OutOfMemoryError`
(`runtime_error`), confirming the kernel's cgroup OOM killer, not the JVM's own
heap accounting, is what fires.

**Test 12 — `memory.max` accounts resident memory, and `MAP_SHARED` is a trap.**
This is the zero-page edge case from Phase 1, and verifying it surfaced a subtlety
worth recording. The *naive* attempt — `mmap.mmap(-1, size)` with Python's
**default `MAP_SHARED`** — is actually OOM-killed during the read walk: anonymous
`MAP_SHARED` pages are real shmem pages charged to `memory.max` the instant they
are faulted in, **even by a read**. Only `MAP_PRIVATE` gives the textbook
zero-page behaviour: a read fault on a never-written private page maps the kernel's
shared zero page, which is not charged, so a 1 GiB mapping (10× the 100 MiB budget)
walks to completion with resident memory near zero. The boundary this documents:
`memory.max` bounds *physical footprint*, nothing else — not virtual address space,
not untouched allocations. The companion fact (test 11) is that the moment those
pages are **written** they become resident and the limit fires immediately. The
only thing that slips through is memory you allocate but never actually use.

**Test 13 — `max_processes`, originally dead config, now enforced.** Every language
in `configs/languages.yaml` has a `max_processes` budget (c's run step: 64). The
first pass of this suite found that budget was parsed and validated but **never
enforced**: the value was parsed into `Limits.MaxProcesses`
(`internal/language/loader.go`) and clamped on a per-request override
(`internal/api/handlers.go` `effectiveLimits`), but `runner.RunSpec` had **no
`MaxProcesses` field**, so `handlers.go` silently dropped it, and `buildNsjailArgs`
(`internal/runner/nsjail.go`) emitted **no `--cgroup_pids_max`**. The original live
result confirmed the gap: a bounded fork bomb reached 2000 processes and hit its own
safety cap with zero resistance.

**The fix (now in place).** `RunSpec` carries a `MaxProcesses` field, `handlers.go`
plumbs it from `effectiveLimits` into both the build and run specs, and
`buildNsjailArgs` emits `--cgroup_pids_max <max_processes>` uniformly — applied to
every language and to both build and run steps, exactly like the memory limit. The
vendored nsjail writes the value to the child cgroup's `pids.max`
(`external/nsjail/cgroup2.cc`), and `--use_cgroupv2` is now emitted whenever *either*
the memory or the pids limit is set.

**How the limit surfaces — and why the status is `runtime_error`.** Hitting
`pids.max` is unlike hitting `memory.max`. The memory limit makes the kernel
*OOM-kill* the child (SIGKILL → nsjail exit 137 → `memory_exceeded`). The pids limit
*kills nothing*: once the cgroup reaches `pids.max`, the kernel simply fails the next
`fork()`/`clone()` with `EAGAIN`. The sandboxed program observes that failed syscall
and decides what to do; this test's program treats a failed fork as fatal and exits
non-zero, which the API maps to **`runtime_error`**. There is no dedicated
`process_limit_exceeded` status, and the runner could not synthesise one — an
`EAGAIN`-from-`pids.max` is indistinguishable from any other non-zero program exit
(there is no distinct signal or exit code like OOM's 137 to key off). So
`runtime_error` is the correct, honest mapping.

The live result confirms the fix with no ambiguity: the bounded fork bomb's `fork()`
fails with `errno=11` (`EAGAIN`) at **`created=63`** — 1 parent + 63 children = 64
tasks = c's `pids.max` of 64 — in ~12–72 ms, far below the 2000 self-cap, and the run
is reported `runtime_error`. The configured per-language process count, not an
incidental host limit, is what bounds it.

Blast-radius containment still holds independently: the sandbox runs in its own PID
namespace (Group 1, test 5), so even the bounded children die with the namespace when
nsjail exits. The test confirms a trivial run immediately afterward returns normally,
so the service survives.

Normal compilation is unaffected: the compiled-language build steps fork internal
subprocesses (g++/gcc → cc1plus/cc1, as, ld; iverilog → ivlpp/ivl via `system()`),
but their build budgets (100 for c/cpp/java, 50 for verilog) leave ample headroom —
all seven languages' hello-world programs still build and run `accepted`.

### The one-time gap, now closed

Group 3 originally found one real gap — process-count limiting via `max_processes`
was configured but unenforced (test 13) — which has since been fixed (see above): the
fork bomb is now bounded at the configured count via a cgroup v2 `pids.max` limit, and
the test asserts that bound. Tests 11-12 held throughout; test 12 additionally
documents that `memory.max` is a resident-only limit and that `MAP_SHARED` anonymous
pages (unlike `MAP_PRIVATE`) are charged even on read.

### Regression

- `go test ./...` — pass (unit/feature suites unaffected by the new `escapetests`-tagged files).
- `go vet ./...` and `go vet -tags escapetests ./escapetests/...` — clean.

---

## Group 4 — shared-kernel boundary (capabilities + network)

Target: the residual attack surface `docs/security.md` flags *outside* the three
named pillars. The container runs `--privileged` (a broad capability grant) and
the threat model lists both "--privileged" and "network namespace configuration
needs review" as open known limitations. These two tests answer them against the
live sandbox rather than by assumption. Both **held** — and both are positive
findings that tighten a previously-open limitation rather than revealing a gap.

| # | Test | Attempt | Result | Outcome |
|---|------|---------|--------|---------|
| 14 | `TestEffectiveCapabilities` | read `/proc/self/status` Cap* masks; corroborate with `chroot("/")` + `sethostname()` (C) | uid 0 but **all five masks empty** (`CapInh/CapPrm/CapEff/CapBnd/CapAmb = 0000000000000000`); `chroot`/`sethostname` → `EPERM` (errno 1), program reaches `AFTER` (not seccomp-killed) | **Held (expected) — capabilities fully dropped despite `--privileged`** |
| 15 | `TestOutboundNetworkBlocked` | non-blocking `connect()` to `8.8.8.8:53` + `select(5s)` (C); corroborate via `/proc/net/dev` + `/proc/net/route` (py3) | `connect()` fails **immediately** with `ENETUNREACH` (errno 101) in ~0 ms; only `lo` interface, **0 route entries** | **Held (expected) — isolated, empty network namespace** |

### Notes on individual tests

**Test 14 — `--privileged` does not reach inside the sandbox.** The key
distinction the notes anticipated: `--privileged` grants capabilities to the
**container**, but nsjail drops them for the **child** independently. It does:
the sandboxed process runs as uid 0 (root) yet every capability mask is empty.
The most important of the five is **`CapBnd` (the bounding set) being empty** —
that caps what the process could ever acquire, so it can never *regain* a
capability even by exec'ing a setuid-root binary; root here is genuinely
powerless. The corroboration removes any doubt that the zeros are cosmetic:
`chroot("/")` (needs `CAP_SYS_CHROOT`) and `sethostname()` (needs
`CAP_SYS_ADMIN`) both return `EPERM`, and because neither syscall is on the
seccomp deny-list the program runs *past* them to print `AFTER` — so the `EPERM`
is a true capability denial, not a Group-2 SIGSYS kill. `NoNewPrivs:1` and
`Seccomp:2` (filter mode) also show in the same dump, consistent with Group 2.
This is the inverse of a gap: the `--privileged` known limitation is materially
narrower than the threat model assumed — the *container* is privileged, the
*sandbox* is capability-less.

**Test 15 — outbound network is isolated, not merely unrouted.** The non-blocking
connect + `select` design distinguishes the three outcomes the notes called out
(immediate fail / success / hang). The result is the cleanest of the three:
`connect()` to `8.8.8.8:53` fails **immediately** with `ENETUNREACH` (errno 101)
in ~0 ms — not a hang (no routing that times out) and not a success. The
corroboration shows why: the sandbox's network namespace contains **only the
loopback device** (`/proc/net/dev` lists just `lo`, and `/sys/class/net` does not
exist) and an **empty routing table** (`/proc/net/route` has zero entries — not
even a default route). There is simply nowhere for a packet to go.

Like Group 1's PID-namespace finding (test 5), this isolation is **inherited from
nsjail's default**, not explicitly configured: nsjail clones a fresh network
namespace unless `--disable_clone_newnet` is passed, and `buildNsjailArgs`
(`internal/runner/nsjail.go`) emits no network flag at all — the comments there
("no network in the jail") are descriptive, not enforcing. So, exactly as with
the PID namespace, this would silently regress if `--disable_clone_newnet` were
ever added to the argument builder.

### No new gaps — two open limitations tightened

Group 4 surfaced no failures. Better than that, it converts two of
`docs/security.md`'s open known limitations from "needs review" into reviewed,
test-backed positive findings: `--privileged` grants the container broad
capabilities but the sandboxed child has **none** (test 14), and outbound network
is **isolated** by a fresh, empty network namespace (test 15). Both rest on
nsjail defaults (capability drop; netns clone) rather than explicit flags — worth
recording so neither silently regresses.

### Regression

- `go test ./...` — pass (unit/feature suites unaffected by the new `escapetests`-tagged files).
- `go test -tags escapetests -v ./escapetests/...` — all 15 pass.
- `go vet ./...` and `go vet -tags escapetests ./escapetests/...` — clean.
