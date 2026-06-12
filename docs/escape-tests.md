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
`BEFORE` but never `AFTER`. All five **held**.

| # | Test | Attempt | Result | Outcome |
|---|------|---------|--------|---------|
| 6 | `TestSeccompPtrace` | `ptrace(PTRACE_TRACEME)` | SIGSYS-killed at the call → `runtime_error`; stdout `"BEFORE\n"`, no `AFTER` | **Held (expected)** |
| 7 | `TestSeccompUnshareNewuser` | `unshare(CLONE_NEWUSER)` | SIGSYS-killed → `runtime_error`; `"BEFORE\n"`, no `AFTER` | **Held (expected)** |
| 8 | `TestSeccompUmount` | `umount2(getcwd())` on the work-dir bind mount | SIGSYS-killed → `runtime_error`; `"BEFORE cwd=/tmp/goboxd-…\n"`, no `AFTER` | **Held (expected)** |
| 9 | `TestSeccompSetns` | `open(/proc/self/ns/mnt)` then `setns(fd,0)` | open succeeds (`fd=3`); `setns` SIGSYS-killed → `runtime_error`; `"BEFORE fd=3\n"`, no `AFTER` | **Held (expected)** |
| 10 | `TestSeccompForkAllowed` | `fork()` + `waitpid()` (negative control) | child exits 7, parent prints `FORK_OK`, exit 0 → `accepted` | **Held (expected)** |

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

### No new gaps

Unlike Group 1's PID-namespace finding, the seccomp group surfaced nothing beyond
what Phase 1's policy and `docs/security.md` already document: every syscall the
deny-list names is killed, the flag-conditional `unshare` rule fires on
`CLONE_NEWUSER` as written, and the one explicitly-allowed primitive (fork/clone)
works. The deny-list semantics described in `configs/seccomp.policy` and the
threat model match observed behaviour exactly.

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
exit 137 → API `memory_exceeded`). Tests 11-12 hold. **Test 13 surfaces a real
gap: `max_processes` is never enforced.**

| # | Test | Attempt | Result | Outcome |
|---|------|---------|--------|---------|
| 11 | `TestMemoryBombResidentPy3` / `…Java` | allocate + touch every page until the budget is crossed (py3 100 MiB, java 512 MiB) | OOM-killed mid-allocation: py3 at ~90 MiB, java at ~450 MiB → `memory_exceeded` | **Held (expected)** |
| 12 | `TestMemoryBombZeroPage` | `mmap.mmap(-1, 1 GiB, MAP_PRIVATE)` then read-only walk (10× py3's budget) | completes `accepted` in ~210 ms — untouched private pages map the shared zero page, never charged to `memory.max` | **Held (documents a boundary)** |
| 13 | `TestForkBombProcessLimit` | bounded fork bomb, children sleep (linear, self-capped at 2000) | reached **2000 processes with zero resistance** (`CAP_REACHED`); no pids limit fired; service still responsive after | **Did NOT hold — `max_processes` unenforced** |

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

**Test 13 — `max_processes` is dead config (a real gap).** Every language in
`configs/languages.yaml` has a `max_processes` budget (c's run step: 64), and a
pre-run code audit shows the value is parsed and even validated but **never
enforced**:

- parsed into `Limits.MaxProcesses` (`internal/language/loader.go`) and clamped on
  a per-request override (`internal/api/handlers.go` `effectiveLimits`), but
- `runner.RunSpec` has **no `MaxProcesses` field** — only `WallTimeSec` and
  `MemoryKB` — so `handlers.go` silently drops the value when constructing the
  spec, and
- `buildNsjailArgs` (`internal/runner/nsjail.go`) emits **no `--cgroup_pids_max`
  and no `--rlimit_nproc`**. The vendored nsjail fully supports `--cgroup_pids_max`
  (`external/nsjail/cgroup2.cc` writes `pids.max`); the runner simply never passes
  it.

The live result confirms the gap with no ambiguity: a bounded fork bomb (only the
parent forks; children sleep — linear growth, not exponential) **created 2000
processes and hit its own safety cap with zero resistance**, in under half a second.
Nothing limited the count: not a pids cgroup, not memory, not the wall clock. A
real *recursive* fork bomb would be bounded only by host memory / wall time / the
kernel's `pid_max` — never by the per-language `max_processes`.

What **does** hold is blast-radius containment, not the count limit. The sandbox
runs in its own PID namespace (Group 1, test 5), so when nsjail exits or hits the
wall limit it tears the namespace down and every spawned child dies with it — the
bomb cannot outlive the request or reach host processes. The test confirms this: a
trivial run immediately afterward returns normally, so the service survived. The
fix is small and mechanical — add a `MaxProcesses` field to `RunSpec`, plumb it
from `effectiveLimits`, and emit `--cgroup_pids_max` in `buildNsjailArgs` — but
until then `max_processes` provides **no** protection, and that is now recorded in
`docs/security.md`'s known limitations.

### New gap found

Unlike Groups 1-2 (which held everywhere), Group 3 found one real gap: process-count
limiting via `max_processes` is configured but unenforced (test 13). It is logged
loudly in the test output and added to `docs/security.md`. Tests 11-12 held; test 12
additionally documents that `memory.max` is a resident-only limit and that
`MAP_SHARED` anonymous pages (unlike `MAP_PRIVATE`) are charged even on read.

### Regression

- `go test ./...` — pass (unit/feature suites unaffected by the new `escapetests`-tagged files).
- `go vet ./...` and `go vet -tags escapetests ./escapetests/...` — clean.
