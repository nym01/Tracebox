# Tracebox — Phase 2 Security Audit Findings (Red-Team)

Read-only adversarial review of the sandbox, looking for gaps **beyond** the 15
escape tests in `escapetests/` (documented in `docs/escape-tests.md`). The lens
is: *I have arbitrary code execution inside the sandbox for each of the 7
languages — what can I reach?* Nothing here is fixed; this is a findings memo to
review and discuss.

Where a finding was cheap and non-destructive to verify, it was run against the
**live** sandbox (`/run`, same path the escape suite uses). Those are marked
**[verified live]** with the observed output.

## TL;DR — what's new

| # | Finding | Class | Verified |
|---|---------|-------|----------|
| **A** | `clone(CLONE_NEWUSER)` / `clone3` bypass the `unshare(CLONE_NEWUSER)` deny rule → **full capability set regained inside the sandbox** | **FIXED** (was POTENTIAL REAL GAP, high) | **fix verified live** |
| **B** | `io_uring` fully available (not on the deny-list) — kernel attack surface + a syscall channel that runs work on kernel threads, bypassing seccomp | **FIXED** (was POTENTIAL REAL GAP) | fix unit-tested; live test 17 added |
| **C** | No `cpu.max`/cpuset limit → CPU exhaustion amplified by `pids.max` threads × `NumCPU` concurrency | **FIXED** (was POTENTIAL REAL GAP, DoS) | fix unit-tested; live test 18 added |
| **D** | Per-request work dir is bind-mounted writable with **no size quota** → disk-fill DoS | **DOCUMENTED KNOWN LIMITATION** (single-file bound enforced; bulk fill residual) | reasoned; live test 19 added |
| E | Every "interpreted" runtime can load a native `.so` from the writable work dir → the escape-suite premise that "only C can issue raw syscalls" is false; findings A/B are reachable from all 7 languages | **ADDRESSED** (doc correction) | partially live |
| **F** | `/proc` leaks host CPU model, total RAM, loadavg, kernel version (not namespaced) | **DOCUMENTED KNOWN LIMITATION** (info leak + side channel; no breach) | **yes, live**; test 20 added |
| G | Build step's `#include "/proc/..."` can echo host info into compiler diagnostics | THEORETICAL / LOW | reasoned |

Finding **A** is the headline: it directly falsifies a claim the current threat
model leans on. `docs/security.md` (and escape test 14) state the sandboxed child
is "root-without-power" because `CapBnd` (the bounding set) is empty, so "the
process can never *regain* a capability." That is true for `unshare`, which the
seccomp policy filters — but **not** for `clone`/`clone3`, which it does not.

---

## A. `clone(CLONE_NEWUSER)` regains a full capability set — **[FIXED — verified live]**

> **STATUS: FIXED.** The seccomp deny-list now covers all three user-/mount-
> namespace creation primitives, not just `unshare`. See **"The fix"** at the end
> of this section for what changed and the verification results. The original gap
> analysis below is retained for the record.

### The gap

`configs/seccomp.policy` filters user-/mount-namespace creation **only on the
`unshare` syscall**:

```
unshare { (unshare_flags & (CLONE_NEWUSER | CLONE_NEWNS)) != 0 }
```

There is **no rule for `clone` or `clone3`**, which can create exactly the same
namespaces. `unshare(flags)` and `clone(flags|SIGCHLD, ...)` are interchangeable
for namespace creation; the deny-list closes one door and leaves the other open.

The whole rationale for the rule (per `docs/decisions.md` D5) is:

> CLONE_NEWUSER is the classic privilege-escalation primitive (a new user
> namespace hands the caller a full capability set inside it).

That escalation is reachable verbatim through `clone`.

### Live proof

A C program calling `clone(child_fn, stack, CLONE_NEWUSER | SIGCHLD, NULL)` and
reading the child's `/proc/self/status`:

```
BEFORE
Uid:    65534   65534   65534   65534
CapInh: 0000000000000000
CapPrm: 000001ffffffffff
CapEff: 000001ffffffffff
CapBnd: 000001ffffffffff      <-- full bounding set; "empty" in test 14
CapAmb: 0000000000000000
CHILD_USERNS_OK
AFTER                          <-- not SIGSYS-killed; clone is not filtered
```

`000001ffffffffff` is **all 40 capabilities**, including `CAP_SYS_ADMIN`.
Compare escape test 14, where the base sandbox shows every mask
`0000000000000000` and `sethostname()` returns `EPERM`. Inside the cloned
namespace that denial **reverses**:

```c
clone(child_fn, stack, CLONE_NEWUSER | CLONE_NEWUTS | SIGCHLD, NULL);
// child:
sethostname("pwned-by-sandbox", 16);   // returns 0
```
```
BEFORE
SETHOSTNAME_OK host=pwned-by-sandbox    <-- EPERM in test 14, succeeds here
AFTER
```

So the "capability-less, root-without-power" property that the threat model
presents as a load-bearing positive finding is **not** load-bearing: untrusted
code restores `CapEff/CapPrm/CapBnd = full` for itself with one unfiltered
syscall. The environment does not stop it — `/proc/sys/user/max_user_namespaces`
is `2147483647` and Debian's `unprivileged_userns_clone` knob is absent
(upstream WSL2 kernel `6.6.114.1`), both **[verified live]**.

### How far does it actually go?

Important caveat so this isn't oversold: the regained capabilities do **not** by
themselves yield a host escape, because the most dangerous follow-on syscalls are
killed by seccomp *regardless of capabilities* (`mount`, `umount`, `setns`,
`bpf`, and `unshare(CLONE_NEWNS)` are SIGSYS-killed whether or not you hold
`CAP_SYS_ADMIN`). An attacker can `clone(CLONE_NEWUSER|CLONE_NEWNS)` to *create* a
mount namespace (also unfiltered), but cannot then call `mount()` to restructure
it. So the immediate blast radius is bounded.

What it does do, and why it's still a real gap:

1. **It defeats a documented control.** The threat model's "the sandbox is
   capability-less" statement is false as written and should not be relied on as
   a layer.
2. **It massively widens kernel attack surface.** Dozens of `CAP_SYS_ADMIN`-gated
   kernel code paths (and cap-gated paths in `keyctl`, `perf_event_open`,
   `ioprio_set`, namespaced `sysfs`, etc. — none on the deny-list) become
   reachable. For a shared-kernel sandbox whose stated real boundary is "kernel
   correctness," handing untrusted code `CAP_SYS_ADMIN` is exactly the precondition
   most LPE/container-escape exploits want.

### Testability

Directly testable, non-destructive — already done above. A natural escape-suite
addition (test 16): C program `clone(CLONE_NEWUSER)`, assert the run is **not**
`runtime_error`-by-SIGSYS *and* fail loudly if the child reports a non-empty
`CapEff`. Note for whoever fixes it later: an argument-filter on `clone` alone is
insufficient — `clone3` passes its flags in a struct via pointer, which seccomp
cannot dereference, so `clone3` would have to be denied outright (or userns
creation blocked at the sysctl/`max_user_namespaces=0` level).

### The fix

Implemented entirely in `configs/seccomp.policy` (kafel), keeping the Phase 1
deny-list approach. Two rules were added so that **every** path to a new user (or
mount) namespace is now closed, matching the existing `unshare` rule:

1. **`clone`** is arg-filtered on its flags exactly like `unshare`:
   `clone { (clone_flags & (CLONE_NEWUSER | CLONE_NEWNS)) != 0 }` → **SIGSYS KILL**.
   The flags are a direct register argument, so seccomp can inspect them. Ordinary
   process/thread creation (`fork`, `pthread_create`, compilers shelling out) never
   sets `CLONE_NEWUSER`/`CLONE_NEWNS`, so it is unaffected (escape test 10 still
   passes).
2. **`clone3`** cannot be arg-filtered (its flags live in a `struct clone_args`
   behind a pointer seccomp cannot dereference). A hard KILL would break glibc
   ≥ 2.34 runtimes that call `clone3` first for `fork`/`posix_spawn`/
   `pthread_create`. Instead it returns **`ERRNO(38)` (ENOSYS)**, which makes glibc
   transparently and permanently fall back to the classic (now-filtered) `clone`
   syscall — its built-in old-kernel path — while a direct `clone3(CLONE_NEWUSER)`
   from an attacker simply fails with `ENOSYS` and creates no namespace.

Why not the alternatives: nsjail 3.4 has no flag to forbid *child* user-namespace
creation (`--disable_clone_newuser` controls nsjail's *own* setup namespace, the
opposite layer); and the sysctl route (`max_user_namespaces=0`) is non-portable
and host-global. Denying `clone`/`clone3` unconditionally was rejected because the
JVM, V8/Node and other multi-threaded runtimes create threads via
`pthread_create` → `clone`, so a blanket kill is a guaranteed regression. The
flag-filter + ENOSYS-fallback pair is the standard container-runtime fix (it is
how Docker's default profile handles `clone3`) and breaks nothing.

### Verification (live, against the rebuilt sandbox)

- **New escape test 16** (`escapetests/seccomp_test.go`,
  `TestSeccompCloneNewuserBlocked`): a C program probes both paths in one run.
  `clone3(CLONE_NEWUSER)` now returns `-1`/`ENOSYS` (no namespace, no
  `CLONE3_USERNS_OK`); `clone(CLONE_NEWUSER)` is SIGSYS-KILLed (no
  `CLONE_USERNS_OK`, no `AFTER` marker, run is `runtime_error`). The pre-fix
  `CapEff: 000001ffffffffff` no longer appears. **Passes.**
- **Escape tests 1–15** re-run unchanged — **all pass** (in particular test 7,
  `unshare(CLONE_NEWUSER)`, and test 10, the `fork`/`clone` negative control,
  confirming the new `clone` filter did not break legitimate process creation).
- **All 7 languages** still run a basic program (py3/bash/js/c/cpp/java/verilog),
  including a multi-threaded Java program that spawns and joins a thread — the JVM
  starts (many threads via `pthread_create` → `clone`) and the spawned thread runs,
  confirming the `clone` filter and `clone3`→ENOSYS fallback do not break threading.

---

## B. `io_uring` is fully available — **[FIXED]**

> **STATUS: FIXED.** The seccomp policy now denies all three io_uring syscalls
> (`io_uring_setup`/`io_uring_enter`/`io_uring_register`) with `ENOSYS`, so no ring
> can be created. See **"The fix"** at the end of this section. The original gap
> analysis below is retained for the record.

### The gap

`io_uring_setup` (syscall 425) is **not** on the deny-list. Live:

```
BEFORE
IO_URING_OK fd=3
AFTER
```

Two problems, both classic deny-list blind spots:

1. **Kernel attack surface.** io_uring has been one of the most prolific sources
   of Linux LPE CVEs in 2022–2024. A conservative deny-list that predates/ignores
   it leaves the entire ring machinery reachable by untrusted code.
2. **Seccomp bypass channel.** io_uring executes submitted operations (read,
   write, openat, connect, …) on **kernel worker threads**, not as syscalls from
   the sandboxed task. A seccomp filter on the task does not see them. Today the
   deny-list doesn't filter I/O syscalls anyway (the mount/net namespaces do the
   containing), so io_uring can't reach anything the namespaces don't already
   block — but it means **the seccomp layer is structurally unable to constrain
   I/O**, so any *future* tightening of the deny-list (e.g. a later allow-list
   hardening pass, the stated Phase-2+ direction) could be silently end-run via
   io_uring unless `IORING_OP_*` are also accounted for.

### Class / testability

POTENTIAL REAL GAP (kernel surface; latent seccomp-bypass). Testable exactly as
above. Adjacent newer/over-broad syscalls in the same "deny-list missed it"
bucket, not individually verified but reachable by the same reasoning:
`userfaultfd` (heap-grooming primitive in many kernel exploits),
`perf_event_open` (LPE history; gated by `perf_event_paranoid`), `add_key` /
`keyctl` / `request_key`. None are on the deny-list.

### The fix

Implemented in `configs/seccomp.policy` (kafel), staying with the Phase-1 deny-list
approach. `io_uring_setup` (425), `io_uring_enter` (426) and `io_uring_register`
(427) are added to the **`ERRNO(38)` (ENOSYS)** block — the same action and the same
reasoning as `clone3`, **not** the KILL block. kafel's bundled syscall table predates
io_uring, so the three numbers are `#define`d as custom syscall ids exactly as
`clone3` already is.

**Why `ENOSYS`, not `KILL`.** ENOSYS makes the kernel report the syscall as
unimplemented — indistinguishable from running on a kernel without io_uring, which is
precisely the condition modern runtimes are written to fall back from. libuv ≥ 1.45
(Node) probes `io_uring_setup` for file I/O and silently reverts to its thread-pool
path when it fails; a hard KILL would turn that probe into a fatal SIGSYS on a future
toolchain bump, a latent regression. On the **current** image (Debian bookworm: Node
18 / libuv 1.44, plus python3/bash/gcc/g++/javac/java/iverilog/vvp) nothing calls
io_uring at all, so there is no behavioural change today; ENOSYS keeps it that way
across upgrades. This mirrors how container runtimes treat probe-and-fallback
syscalls and is consistent with the existing `clone3` rule.

**Why this fully closes the finding.** Both halves of the gap hinge on a ring
existing. Denying `io_uring_setup` means no ring is ever created, so there is no
io_uring kernel machinery for untrusted code to reach (the LPE attack surface) and no
ring through which I/O could be submitted to kernel worker threads (the
seccomp-bypass channel). `io_uring_enter`/`register` are denied too as
belt-and-suspenders; without a ring fd they are unreachable anyway.

The adjacent syscalls noted above (`userfaultfd`, `perf_event_open`, `keyctl`, …)
remain a deny-list residual — the inherent property of a deny-list, already flagged
as a known limitation in `docs/security.md`. They are out of scope for this fix,
which targets the specific, verified io_uring exposure.

### Verification

- **New escape test 17** (`escapetests/seccomp_test.go`,
  `TestSeccompIoUringBlocked`): a C program issues `io_uring_setup` via raw syscall.
  Pre-fix it printed `IO_URING_OK fd=3`; post-fix it must print
  `IO_URING_DENIED ret=-1 errno=38` (no ring), then run **past** the call to `AFTER`
  (ENOSYS is a graceful denial, not a SIGSYS kill — so, unlike tests 6-9, the run is
  not `runtime_error`). `IO_URING_OK` must not appear.
- **Unit:** the policy is handed to nsjail for every language by the existing
  `TestSeccompPolicyPassedToNsjailForEveryLanguage`; no per-language branch, so the
  io_uring rule applies uniformly.
- **Languages:** none of the seven call io_uring on the current image, so all seven
  still build and run normally (the ENOSYS choice guarantees no breakage even if a
  future runtime starts probing it).

---

## C. No CPU limit → CPU-exhaustion DoS amplified by threads × concurrency — **[FIXED]**

> **STATUS: FIXED.** Each run now carries a per-request cgroup v2 `cpu.max` limit,
> sized per language in `configs/languages.yaml`. See **"The fix"** at the end of
> this section.

`buildNsjailArgs` sets `--time_limit` (wall clock) and a cgroup `pids.max`, but
**no `cpu.max` and no cpuset** (`--max_cpus`). Wall-time bounds *elapsed* time,
not *CPU consumed*:

- A single request can run up to its `pids.max` busy threads (py3/java/cpp = 64–100,
  bash = 10) — each pinning a core — for the **entire** wall window (3–10 s).
- The server's global concurrency cap is `runtime.NumCPU()`
  (`internal/api/concurrency.go`), so `NumCPU` such requests run at once.
- Product: a small number of requests can saturate every host core for seconds at
  a time. On a shared host this is a noisy-neighbor / availability problem, and it
  is *not* caught by `memory.max` (CPU spin allocates nothing) or `pids.max`
  (threads stay under budget) or the wall timer (it *is* doing work, just useless
  work).

**Class:** POTENTIAL REAL GAP (DoS, not isolation breach). **Testability:** easy
but mildly antisocial (it burns CPU on the live box); a bounded version — N
threads spinning for the wall window, assert the request still returns and the
service stays responsive — mirrors the fork-bomb test 13. Not run here to avoid
loading the shared host. Fix direction (later): a per-request `cpu.max` quota
and/or cpuset.

### The fix

A per-request cgroup v2 **`cpu.max`** limit, wired exactly like the existing
`memory.max` and `pids.max` limits:

1. **nsjail flag.** nsjail 3.4 has `--cgroup_cpu_ms_per_sec N`: it writes
   `cpu.max = "<N*1000> 1000000"` on the child's cgroup
   (`external/nsjail/cgroup2.cc` `initNsFromParentCpu`), i.e. the cgroup may use *N*
   milliseconds of CPU in each 1-second period — *N*/1000 cores' worth. (Checked the
   vendored source for the exact flag name and behaviour rather than assuming; there
   is no separate cpuset flag, and cpuset is not needed — bandwidth throttling is
   what bounds the DoS.)
2. **Config + plumbing.** A new `cpu_ms_per_sec` field on the per-step `limits` in
   `configs/languages.yaml` flows through `language.Limits` → `effectiveLimits`
   (clamped so a request can only *tighten* it, like the others) →
   `runner.RunSpec.CPUMsPerSec` → `buildNsjailArgs`, which emits
   `--cgroup_cpu_ms_per_sec` and turns on `--use_cgroupv2`. Applied uniformly to every
   language and to both the build and run steps.
3. **Values and reasoning.** py3/bash run = 1000 (1 core); cpp/c (build+run),
   js run, verilog (build+run) = 2000 (2 cores); java (build+run) = 4000 (4 cores,
   the most thread-parallel runtime: JIT + GC). The key property that makes these safe
   is that **`cpu.max` throttles, it does not kill**: a value set "too low" can only
   ever *slow* a program, never change its result. Normal compiles/runs use little
   *sustained* CPU and finish well within `wall_time_s`, so the cap never bites them —
   only a CPU-spinner is throttled, and it is still ended by the wall limit
   (→ `time_exceeded`), exactly as before. The values are therefore sized generously
   (≥ 1 core, 4 for the JVM) to leave **zero** risk of a false `time_exceeded` on
   legitimate multi-threaded startup, while still bounding each request to a few cores
   so that `pids.max` threads × `NumCPU` concurrency can no longer multiply into a
   host-wide CPU-exhaustion DoS.

Why a per-request bandwidth cap rather than a global one: the amplification is
*per request* (one request × up to `pids.max` busy threads), and the server already
caps concurrency at `NumCPU`, so bounding each request to *k* cores bounds total CPU
demand at *k* × `NumCPU` instead of `min(pids.max, NumCPU)` × `NumCPU`.

### Verification

- **New escape test 18** (`escapetests/cgroup_test.go`, `TestCpuExhaustionBound`):
  a 16-thread C busy-spinner is cleanly bounded (`time_exceeded`, not a crash/hang)
  and the service stays responsive afterward; a finite single-threaded py3 compute
  (the positive control) still returns `accepted`, proving the cap does not throttle
  normal work into a false `time_exceeded`. (The cap's *primary* effect — limiting
  host cores consumed — is not observable through the HTTP API, so it is covered by
  the unit tests below plus nsjail's `cpu.max` write.)
- **Unit:** `TestCgroupCpuLimitPassedToNsjail`,
  `TestCgroupCpuLimitAppliedToEveryLanguage`, `TestCgroupCpuLimitOmittedWhenUnset`
  and an extended `TestCgroupUseCgroupv2EmittedOnce` (all in
  `internal/runner/nsjail_test.go`), plus `TestEffectiveLimitsCPUMsPerSec`
  (`internal/api/limits_test.go`) for the tighten-only clamp.
- **Languages / escape tests 1-17:** `go build ./...`, `go vet ./...`,
  `go test ./...` and `go vet -tags escapetests ./escapetests/...` all pass; the
  generous per-language values mean no normal build/run is throttled into a false
  failure.

---

## D. Writable work dir has no size quota → disk-fill DoS — **[DOCUMENTED KNOWN LIMITATION]**

> **STATUS: DOCUMENTED KNOWN LIMITATION (partial mitigation in place).** The
> *single-file* case is already bounded by nsjail's default `rlimit_fsize` (1 MiB);
> the *many-files* bulk-fill case has no proportionate code fix given the
> architecture, so it is documented with an operational mitigation. See **"The
> determination"** at the end of this section.

The per-request work dir is `os.MkdirTemp("", "goboxd-*")`
(`internal/api/handlers.go`), i.e. under the container's `/tmp` (overlay/disk,
not memory), and is bind-mounted **writable** into every sandbox
(`isolatedArgs` → `--bindmount spec.WorkDir`). There is no quota on it.

- A program can write until the container's writable layer / `/tmp` fills, which
  can break the goboxd server itself (it `MkdirTemp`s and writes a source file per
  request — both fail once the fs is full) → service-wide DoS.
- nsjail's **default** `rlimit_fsize` (1 MiB — see "implicit defaults" below)
  caps any *single* file, but not the *number* of files, so a create→write→close
  loop fills the disk regardless.
- Not charged to `memory.max`: these are disk-backed pages, not the cgroup's
  resident anonymous memory. (If the host's `/tmp` were tmpfs-backed the writes
  *would* be charged and OOM-kill instead — so the outcome is deployment
  dependent, which is itself worth pinning down.)
- `defer os.RemoveAll(tmpDir)` reclaims it after each request, so the pressure is
  transient per request — but concurrent/long-wall requests overlap.

The build profiles' `--tmpfsmount /tmp` is *not* the same exposure: that tmpfs
takes nsjail's small default size and is torn down with the mount namespace.

**Class:** POTENTIAL REAL GAP (DoS). **Testability:** easy but destructive (fills
disk) — describe, don't run. Fix direction: size-limit the work-dir mount / set a
disk quota, or set an explicit small `rlimit_fsize` *and* cap inode/file count.

### The determination

Investigated both fix options the finding lists and concluded a proportionate code
fix for the *general* (many-files) case is not available without re-architecting,
so this is documented as a known limitation with a partial mitigation already in
force and a recommended operational mitigation.

**Why a size-limited tmpfs work dir does not fit.** The work dir cannot be swapped
for a `--mount none:DST:tmpfs:size=N` mount, because **build and run are separate
nsjail invocations that share state through this directory**: the compiler writes
the artifact (`./solution`, `*.vvp`, `*.class`) in the build step and the run step —
a *different* nsjail process — executes it. A per-invocation tmpfs would be empty in
the run step, so the artifact would vanish between build and run. The directory must
be a host-backed bind mount for that hand-off to work. (For the interpreted
languages the source file is likewise written to the host dir by the Go server
*before* nsjail starts, so it too must be a host bind mount to be visible.) A tmpfs
would also charge writes against `memory.max` rather than disk, conflating two
limits — which the finding explicitly warns against.

**Why a per-request disk quota is over-engineering.** A real per-request quota needs
either a loop-mounted filesystem of fixed size per request (mkfs + mount/umount per
request — heavyweight, and the mount syscalls are seccomp-killed inside the sandbox,
so it would have to be orchestrated by the server) or project/user quotas on the
backing filesystem (filesystem-type-dependent, needs quota tooling in the image).
Both are disproportionate to a DoS-class (not isolation-breach) finding.

**What IS already enforced (the partial mitigation).** nsjail's default
`rlimit_fsize` of **1 MiB** caps the size of any *single* file: a write past 1 MiB
returns `EFBIG`, or the writer is `SIGXFSZ`-terminated. So the unbounded-*single-file*
variant is already closed — verified by **new escape test 19**
(`TestSingleFileWriteBounded`): a program appending to one file never reaches a large
size and the service survives. The residual is purely the **create-many-1-MiB-files**
loop, whose total is unbounded by any rlimit. Per-request `os.RemoveAll` reclaims the
dir after each request, and concurrency is `NumCPU`-bounded, so the pressure is
transient and bounded in *width*, just not in per-request *total*.

**Recommended operational mitigation (document, deploy-side).** Bound the blast
radius at the container, where it is cheap and effective:

- Mount the container's `/tmp` (where the work dirs live) as a **size-limited
  tmpfs**, e.g. `docker run --tmpfs /tmp:size=512m …` or a sized volume. A fill then
  hits the tmpfs cap and the offending request's writes fail — it cannot reach the
  host disk, and other tenants are unaffected once that request is reaped. (Sizing
  `/tmp` as tmpfs also makes the disk pressure charge to container memory, which is
  itself bounded by the container's memory limit.)
- Independently, **monitor/alert on host (and container) disk usage** and keep the
  per-request `os.RemoveAll` cleanup (already present) so leaked dirs cannot
  accumulate.

This mirrors the threat model's overall stance: the strong per-request isolation is
in the sandbox, and a few availability concerns are addressed operationally at the
container boundary (the same place `--privileged` and concurrency live).

---

## E. "Interpreted" languages can reach raw syscalls too (threat-model correction) — **[ADDRESSED]**

> **STATUS: ADDRESSED (documentation).** This was a documentation correction, not a
> code gap. `docs/security.md`'s seccomp section already treats the deny-list as the
> uniform backstop for *every* language (it carries no "only C can issue syscalls"
> claim), and a sentence has been added there making explicit that any runtime can
> load a native `.so` from the writable work dir — so seccomp, not mount minimalism,
> is what backstops findings A/B across all seven languages. The narrower per-test
> note in `docs/escape-tests.md` (test 8) is about the *mount* syscalls specifically
> being unreachable from the interpreted languages' *built-in* facilities (py3 has no
> ctypes), which remains true and is left as-is; the broader "raw syscalls via a
> loaded `.so`" point is what this finding corrects, and that now lives in
> `docs/security.md`.


`docs/escape-tests.md` repeatedly asserts C is "the only one of the seven runtimes
that can issue a raw syscall directly," and that py3 in particular can't (ctypes is
absent because `libffi.so.8` isn't in its mount profile). That conclusion is too
strong, and it matters because findings A and B are *only* interesting if
attackers can issue syscalls:

- **py3:** the work dir is `cwd` and on `sys.path`. A program can write
  `evil.cpython-311-x86_64-linux-gnu.so` (bytes embedded in the 256 KiB source,
  base64-decoded) and `import evil` — Python's import machinery `dlopen`s it via
  the loader, **not** via ctypes/libffi, so the missing-`libffi` barrier doesn't
  apply. Native code → raw syscalls.
- **js:** `process.dlopen(module, '/work/evil.so')` loads a native addon written
  to the work dir. (`child_process` is separately neutered — no `/bin/sh` or other
  binary is mounted — but dlopen needs no external binary.)
- **java:** `System.load("/work/evil.so")` via JNI; or simply JNI/`Unsafe`.
- **cpp/c:** native already.

The attacker has to *supply* the `.so` (they can't compile one inside py3/js/java),
but they fully control the source bytes, and a minimal `.so` is small. So the
practical reality is: **all 7 runtimes can reach raw syscalls**, and therefore the
seccomp deny-list — not mount minimalism — is the real backstop for findings A/B
across every language, not just C.

**Class:** THEORETICAL on its own (no escape; native code is still inside the same
namespaces/seccomp/cgroups), but it's an **amplifier**: it widens A and B from
"C-only" to "every language." **Testability:** moderate (need a prebuilt `.so`
blob in the payload). Verified the *mechanism* indirectly — py3 reading arbitrary
`/proc` files and running fine shows the runtime is unconstrained beyond the
mount/seccomp layers.

---

## F. `/proc` leaks host facts and a co-tenant side channel — **[DOCUMENTED KNOWN LIMITATION]**

> **STATUS: DOCUMENTED KNOWN LIMITATION.** Masking the non-namespaced `/proc`
> entries cannot be done reliably without risking runtime breakage (the JVM and
> V8/Node read `/proc/cpuinfo`, and the JVM may read `/proc/meminfo`, for ergonomic
> sizing), and the exposure is an info-leak, not a breach. So it is documented with
> per-file reasoning and the recommended stronger fix (gVisor). See **"The
> determination"** at the end of this section.

`/proc` is mounted (read-only) in the sandbox. Several files are **not**
namespaced and reflect the host:

```
cpuinfo:  model name : 11th Gen Intel(R) Core(TM) i5-1135G7 @ 2.40GHz
meminfo:  MemTotal: 7977068 kB | MemFree: 6797200 kB | MemAvailable: 7170032 kB
loadavg:  0.00 0.00 0.00 3/295 1
osrelease: 6.6.114.1-microsoft-standard-WSL2
```

- **Fingerprinting:** exact host CPU, total RAM, and kernel version are handed to
  untrusted code — useful for selecting a kernel exploit (ties to A/B: knowing the
  kernel narrows which io_uring/userns CVE to try).
- **Side channel:** `/proc/loadavg`, `/proc/stat` and `MemFree`/`MemAvailable`
  fluctuate with *other tenants'* activity, giving a low-bandwidth covert/side
  channel between concurrent sandboxes on the same host.

Good news in the same probe (**[verified live]**): `/sys/fs/cgroup/memory.max` is
**FileNotFoundError** — `/sys` is not mounted, so the child cannot read or raise
its own cgroup limits, and combined with the fresh cgroup namespace, cgroup
self-tampering is **already mitigated**. `/sys/devices/system/cpu/online` is also
absent.

**Class:** THEORETICAL / LOW-to-MEDIUM (info leak + side channel; no direct
breach). **Testability:** trivial, done above. Fix direction (later): mask the
non-namespaced `/proc` entries or run with a hardened proc view.

### The determination

Investigated masking these entries (nsjail can overlay individual files via
`--bindmount_ro fake:/proc/<file>`, or drop procfs entirely with `--disable_proc`)
and concluded a reliable, non-breaking mask is not available across the seven
runtimes, so this is documented as a known limitation. The exposure is information
disclosure / fingerprinting / a low-bandwidth side channel — **no read, write, or
control of anything outside the sandbox** — so the cost/benefit does not favour a
risky runtime-compatibility change. Per file:

- **`/proc/version`** (kernel version string): nothing in any of the seven runtimes
  reads it for behaviour. *Safe* to mask in principle — but on its own it is the
  lowest-value leak, and masking it alone does not change the threat (the kernel
  version is also inferable from syscall behaviour and `uname`, which is not
  filtered).
- **`/proc/loadavg`** (and `/proc/stat`): the side-channel surface. Nothing needs it
  functionally, so it is *maskable*, but a static fake does not remove the channel
  cleanly (other timing/`/proc/meminfo` signals remain) and it is the lowest-severity
  part of the finding.
- **`/proc/cpuinfo`** (CPU model + core count): **risky to mask.** V8/Node
  (`os.cpus()`, libuv) and the JVM read it to size thread pools / GC and JIT
  parallelism; a fake or empty file can yield a zero/!wrong core count and mis-size
  or, in some runtimes, fail. This is the file most likely to break a runtime.
- **`/proc/meminfo`** (total/free RAM): **risky to mask.** In a container the JVM
  prefers cgroup limits for heap ergonomics, but HotSpot still consults
  `/proc/meminfo` on some paths, and other tools read it; a fake value risks
  mis-sized heaps. Left intact for the same compatibility reason.

A *partial* mask (only `version` + `loadavg`, leaving `cpuinfo`/`meminfo`) is
possible and safe, but it removes only the two lowest-value leaks while leaving the
two that actually aid exploit selection (CPU/kernel fingerprint) and co-tenant
sizing — so it is not worth the added mount complexity and the per-file fake-content
maintenance. The clean, complete fix is a sandbox that *synthesises* `/proc` rather
than masking files one by one — **gVisor**, already named in `docs/security.md`
"Strengthening the boundary" as the next step up from a shared-kernel sandbox. Under
gVisor the Sentry presents its own `/proc`, closing this entire class without
per-file fakes. Recommended operational mitigation in the meantime: treat host CPU /
RAM / kernel version as known-to-untrusted-code (they aid exploit selection but are
not themselves a breach), and rely on the kernel-correctness boundary the threat
model already names.

**Verification:** new escape test 20 (`escapetests/filesystem_test.go`,
`TestProcHostInfoLeak`) reads the four files and logs which host facts are visible,
documenting the current behaviour (like test 12's zero-page boundary). It does not
fail on the leak — it only fails if `/proc` stops being mounted at all, which would
change the premise of tests 2 and 5, so a future change to `/proc` handling cannot
pass silently.

---

## G. Build step can read host `/proc` into compiler diagnostics

The build sandboxes (g++, gcc, javac, iverilog) mount `/proc` and have broad,
read-only access to system paths. A malicious source can
`#include "/proc/cpuinfo"` (or any readable file): the compiler chokes on the
non-C content and **echoes lines of it into the error diagnostics**, which the API
returns as `build.stderr`. Same host-info class as F, reached at build time even
for languages whose *run* step has no `/proc` surprises. Minor, but it's a second
path to the same leak and a reminder that the build profiles are deliberately
broad. **Class:** THEORETICAL / LOW.

---

## What I checked and found ALREADY MITIGATED

Recording the negatives so the discussion knows they were looked at, not skipped.

### Filesystem (all 7 languages)
- **Read/write outside the mount set, symlink tricks, `../../` traversal,
  `/proc/self/root|cwd`:** the mount namespace root is a fresh tmpfs populated only
  with the profile's read-only binds; host paths simply don't exist, `/proc/self/root`
  points at the sandbox root, and the root is read-only to the child. Covered by
  escape Group 1 (tests 1–5). A symlink the child creates in the work dir still
  resolves *within* the sandbox namespace — no target to escape to.
- **RO bind mount with a "writable parent":** the parent of every RO bind is the
  tmpfs root, which is read-only to the child (test 3 — writing `/`, `/usr`, `/etc`
  all denied). You cannot shadow a mount by creating a file over it.
- **Remount RO→RW:** `mount`/`umount` are SIGSYS-killed (Group 2, test 8).
- **TOCTOU on the work dir:** each request gets a unique `MkdirTemp` dir; no other
  tenant shares it, and the Go server never reads files back out of it (it captures
  stdio, not artifacts) except `os.RemoveAll`, which does not follow symlinks. No
  cross-request window.

### seccomp
- `ptrace`, `bpf`, `mount`, `umount`, `setns`, `unshare(CLONE_NEWUSER|CLONE_NEWNS)`,
  `kexec_load`, `init/finit/delete_module`, `reboot`, `swapon/off` — all killed
  (Group 2). The gaps are the syscalls the deny-list *doesn't* name (A, B above),
  which is the inherent risk of a deny-list and is already flagged as a known
  limitation in `docs/security.md`.
- `process_vm_readv`/`writev`: not on the deny-list, but the PID namespace leaves
  no sibling process to target (Group 1, test 5), so there's nothing to read.
  `memfd_create`+`execve`: fileless execution, but seccomp/namespaces/caps persist
  across `execve` (NoNewPrivs), so it's capability-parity, not an escape.

### Network / IPC / devices
- Fresh, empty network namespace (only `lo`, no routes) — Group 4, test 15.
- AF_UNIX (filesystem sockets): none of the host's sockets are in the mount
  namespace, so none are reachable. Abstract unix sockets are scoped to the net
  namespace → isolated. SysV/POSIX shm: fresh IPC namespace (nsjail default) and no
  `/dev/shm` mounted → no shared memory with host. `/dev` is minimal (null/zero/
  random) → no device-file signaling channel.

### Resource limits beyond the documented ones
- **FD exhaustion:** nsjail's default `rlimit_nofile` (32) caps it.
- **Memory:** `memory.max` + `swap.max=0` (resident) — Group 3, tests 11–12.
- **Process/thread count:** `pids.max` — Group 3, test 13.
- **cgroup self-escape:** `/sys/fs/cgroup` not mounted (**[verified live]**) + fresh
  cgroup namespace → child can't edit its own limits.

### Build-vs-run asymmetry (the broad build profiles)
- A build-time RCE (e.g. via a javac annotation processor or a Verilog `system()`
  injection) lands in **the same** namespaces + seccomp + dropped caps + cgroup
  limits as the run step. The build profile's extra exposure is (a) more
  **read-only, trusted** system files (`/usr/include`, the JDK, the ivl base dir)
  and (b) a small writable tmpfs `/tmp` that's torn down with the mount namespace.
  Neither is a meaningfully larger *escape* surface. The flag allow-lists also block
  the obvious build-time levers: javac's `-processorpath`/`-processor` aren't in
  `["-g","-Xlint:all","-Xlint:none"]`; gcc/g++ `@responsefile`, `-I`, `-l`,
  `-specs=` etc. don't match `["-O*","-Wall","-Wextra","-std=*"]`; and the source
  filename is server-fixed, so no shell injection into iverilog's `system()` command
  lines via the filename. Verilog `$system()` at **run** time has no `/bin/sh`
  mounted (decision D4) so it can't reach a shell. These are working as intended.

---

## Per-language quick read

- **py3** — base case for filesystem (Group 1). New: can `import` a native `.so`
  from the work dir (E), so A/B are reachable here, not just in C. `/proc` leak (F).
- **c / cpp** — native already; the vehicle that *proved* A and B. Build profile is
  broad but RO/trusted; flag allow-list holds.
- **bash** — most constrained: 10-process budget, no external binaries mounted, no
  `/bin/sh`-reachable tools. `child_process`-style abuse is dead on arrival.
- **js** — `child_process` neutered (no binaries mounted) but `process.dlopen` of a
  work-dir `.so` gives native code (E) → A/B reachable.
- **java** — JNI/`System.load` of a work-dir `.so` → native code → A/B. Build step
  (javac on the JVM, writable `/tmp`, full JDK RO) is the broadest profile but
  bounded as in "build-vs-run" above; annotation-processor abuse is blocked by the
  flag allow-list and the single-source-file classpath.
- **verilog** — `$system()` can't reach a shell at run time (no `/bin/sh` in the run
  profile, D4). Build step *does* mount `/bin/sh` for iverilog's `system()`; injection
  surface is low (fixed filename, narrow flag allow-list) but is the one spot worth a
  closer look if VPI-module loading (`-m`/source pragmas) can be coerced — not
  confirmed either way here.

---

## Suggested follow-ups — status

The original triage list, updated with what was done:

1. **A (clone userns)** — **DONE.** `clone` arg-filtered on `CLONE_NEWUSER`/
   `CLONE_NEWNS` and `clone3` given `ENOSYS`; escape test 16 added. (Prior change.)
2. **B (io_uring)** — **DONE.** `io_uring_setup`/`io_uring_enter`/
   `io_uring_register` denied with `ENOSYS`; escape test 17 added. `userfaultfd` /
   `perf_event_open` and the other adjacent newer syscalls remain a deny-list
   residual (see Finding B "The fix") — a candidate for a future allow-list pass.
3. **C (CPU DoS)** — **DONE.** Per-request cgroup v2 `cpu.max` via
   `--cgroup_cpu_ms_per_sec`, sized per language in `configs/languages.yaml`; escape
   test 18 added.
4. **D (disk DoS)** — **DOCUMENTED KNOWN LIMITATION.** Single-file writes are already
   bounded by `rlimit_fsize` (escape test 19); the many-files bulk-fill case has no
   proportionate code fix given the host-bind-mount architecture, so it is documented
   with a container-side operational mitigation (size-limit `/tmp`, monitor disk).
5. **F / G (info leak)** — **F DOCUMENTED KNOWN LIMITATION.** Masking
   `/proc/cpuinfo`/`meminfo` risks JVM/Node runtime breakage; `version`/`loadavg` are
   maskable but low-value. Documented with per-file reasoning and gVisor as the clean
   fix; escape test 20 records the current state. G unchanged (THEORETICAL / LOW).
6. `docs/security.md` — **DONE** (in the Finding A change): the "capability-less"
   claim now notes it holds only because `unshare`/`clone`/`clone3` are all filtered;
   this round adds the seccomp io_uring rule, the cgroup CPU pillar, and the D/F
   known limitations.

## Appendix — implicit nsjail default rlimits (undocumented defense-in-depth)

`buildNsjailArgs` overrides only `--rlimit_as max`. It does **not** set
`rlimit_fsize`, `rlimit_nofile`, `rlimit_nproc`, or `rlimit_cpu`, so nsjail's
built-in defaults apply to every sandbox: `rlimit_fsize` ≈ 1 MiB (caps single-file
size — see D), `rlimit_nofile` = 32 (caps FD count), `rlimit_nproc` = 1024 (a
coarse backstop behind the tighter cgroup `pids.max`), `rlimit_cpu` = 600 s (moot
behind the 3–10 s wall limit). These are real, load-bearing limits that
`docs/security.md` doesn't mention — and, like the PID/net-namespace defaults the
docs already flag, they'd **silently change** if anyone later passes explicit
`--rlimit_*` flags to the argument builder. Worth documenting so they aren't lost.
