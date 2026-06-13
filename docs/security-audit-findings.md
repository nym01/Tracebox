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
| **B** | `io_uring` fully available (not on the deny-list) — kernel attack surface + a syscall channel that runs work on kernel threads, bypassing seccomp | **POTENTIAL REAL GAP** | **yes, live** |
| C | No `cpu.max`/cpuset limit → CPU exhaustion amplified by `pids.max` threads × `NumCPU` concurrency | POTENTIAL REAL GAP (DoS) | reasoned |
| D | Per-request work dir is bind-mounted writable with **no size quota** → disk-fill DoS | POTENTIAL REAL GAP (DoS) | reasoned |
| E | Every "interpreted" runtime can load a native `.so` from the writable work dir → the escape-suite premise that "only C can issue raw syscalls" is false; findings A/B are reachable from all 7 languages | THEORETICAL → amplifier | partially live |
| F | `/proc` leaks host CPU model, total RAM, loadavg, kernel version (not namespaced) | THEORETICAL / LOW-MED (info leak + co-tenant side channel) | **yes, live** |
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

## B. `io_uring` is fully available — **[verified live]**

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

---

## C. No CPU limit → CPU-exhaustion DoS amplified by threads × concurrency

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

---

## D. Writable work dir has no size quota → disk-fill DoS

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

---

## E. "Interpreted" languages can reach raw syscalls too (threat-model correction)

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

## F. `/proc` leaks host facts and a co-tenant side channel — **[verified live]**

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

## Suggested follow-ups (not done — for discussion)

These are *findings to triage*, deliberately not implemented (this was read-only):

1. **A (clone userns)** — highest priority; it falsifies a stated control. Decide
   between sysctl (`max_user_namespaces=0` in the container) vs. seccomp changes
   (deny `clone3`, arg-filter `clone`'s `CLONE_NEW*`). Add escape test 16.
2. **B (io_uring)** — add `io_uring_setup`/`io_uring_enter`/`io_uring_register`
   (and reconsider `userfaultfd`, `perf_event_open`) to the deny-list; add a test.
3. **C / D (DoS)** — add `cpu.max`/cpuset and a work-dir size/inode quota.
4. **F / G (info leak)** — mask non-namespaced `/proc` entries.
5. Update `docs/security.md`: the "capability-less / `CapBnd` empty" claim is only
   true until the child calls `clone(CLONE_NEWUSER)`; the deny-list's `unshare`
   rule should be described as *incomplete* (clone/clone3 uncovered).

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
