# Tracebox — Phase 7 Stage 4 Security Assessment (gVisor backend)

Adversarial assessment of the **gVisor (`runsc`) backend** (`GvisorRunner`,
`GOBOXD_RUNNER=gvisor`; Stages 1–3 — all 7 languages, Finding F fixed,
strace-based audit trail). It is the gVisor counterpart of the nsjail red-team
memo in `docs/security-audit-findings.md`, and is primarily an
**investigation/findings** document, not a port of the 21 nsjail escape tests.

Where a check was cheap and non-destructive it was run against a **live, separate
gVisor instance** (a `tracebox` container with `GOBOXD_RUNNER=gvisor` on a private
port, never the shared `:8080`), the same black-box `/run` path the escape suite
uses. Those are marked **[verified live]** with the observed output. One class of
check (deliberately stressing the sentry to its failure point) was held back on
purpose — see §2.1.

## The core reframe — why this is not a 1:1 port of the 21 tests

The 21 nsjail escape tests probe **Linux kernel primitives** (namespaces, seccomp,
cgroups): *"can the sandboxed process do X to the host kernel?"* Under gVisor the
guest **never talks to the host kernel directly** — every guest syscall is trapped
(systrap: seccomp + signals) and serviced by the **sentry**, gVisor's userspace
reimplementation of the kernel. So many of the 21 tests' exact questions are
**category errors** under gVisor:

- *"Create a user namespace and gain `CAP_SYS_ADMIN`"* — gain it **where**? In the
  sentry's synthesized user model, which confers power only over the sentry's own
  emulated resources, not the host. **[verified live]** below: `unshare(CLONE_NEWUSER)`
  actually **succeeds** under gVisor (it is SIGSYS-*killed* under nsjail), and that
  is *not* a regression — the resulting "capabilities" reach nothing on the host.
- *"`mount`/`umount`/`setns` the host filesystem"* — those syscalls are handled by
  the sentry against its own VFS; there is no host mount table to manipulate.

The boundary moved. Under nsjail the boundary is *host seccomp + namespaces +
dropped caps* and the characteristic failure is a **kernel syscall gap** (Finding A's
class). Under gVisor the boundary is *the sentry itself*, and the characteristic
failure is a **sentry implementation bug** (§3). This document classifies the 21
tests against the new boundary (§1), then asks the questions that are actually
meaningful for the sentry architecture (§2), notes the different vulnerability class
(§3), and compares the two backends head-to-head (§4).

---

## TL;DR

| # | Finding | Class | Verified |
|---|---------|-------|----------|
| **Finding F** | The whole `/proc` host-info leak class is **closed** by the sentry's synthesised `/proc` — kernel version, MemTotal, loadavg, `/proc/stat`, uptime all synthetic; env shows no host inheritance | **FIXED** (the reason this backend exists) | **[verified live]** |
| **G1** | **No `pids`/task limit on the guest** (`gvisorResources` defers `MaxProcesses`). A guest spawns far more tasks than nsjail's `pids.max` (600 threads OK vs nsjail's 64). Contained *indirectly* by the memory cap + wall limit; the residual is a sentry-pressure availability concern | **REAL GAP (DoS-class), narrower than it looks** | **[verified live]**, bounded |
| **G2** | `/proc`+`/sys`+env synthesis is **comprehensive**; the only residual host leak is `/proc/cpuinfo` `cpu family`/`model`/`flags`/`MHz` (microarchitecture fingerprint) — the documented caveat, nothing new | **DOCUMENTED KNOWN LIMITATION** (info leak, no breach) | **[verified live]** |
| **G3** | The Stage-3 `--strace --debug` audit log grows **unbounded per-run** (~206 B/openat; 78 MB for 400k opens) with no size cap, on host-backed `/tmp` — a new disk/parse-memory DoS vector distinct from guest limits; **but** cleaned per-run (`defer RemoveAll`) and width-bounded by concurrency | **NEW, DoS-class** | **[verified live]** |
| G4 | Cross-mechanism parity: **memory** (OOM→`memory_exceeded`) and **network** (`ENETUNREACH`) enforcement are equivalent-or-stronger; the one asymmetry is the looser memory cap (guest budget + 512 MB headroom) and G1's missing pids cap | **NEITHER WEAKER except G1** | **[verified live]** |
| Tests 6–9,16 | The seccomp **deny-list** tests are **N/A** (category error): the guest has `Seccomp: 0`, the sentry is the filter; denied-under-nsjail ops are simply *handled* by the sentry, reaching no host surface | classification | **[verified live]** |

The single most useful artifact is the **§4 comparison table**.

---

## 1. Classifying the 21 nsjail escape tests against the gVisor boundary

The suite is tests **1–20** (21 test functions — test 11 has a py3 and a java
variant). For each, the question is whether it is **N/A** (a category error under
gVisor — the concept doesn't exist for the guest), **EQUIVALENT** (same concern,
different enforcement, and gVisor enforces it), or **STILL RELEVANT** (the concern
applies to gVisor too, possibly via a different vector).

| # | Test | nsjail mechanism probed | gVisor verdict | Why |
|---|------|------------------------|----------------|-----|
| 1 | `TestReadEtcPasswd` | mount-ns minimal root | **EQUIVALENT** | gVisor runs against a static read-only rootfs; `/etc/passwd` of the host is not in the tree. The sentry's VFS only sees the rootfs + binds. |
| 2 | `TestReadOutsideBindMounts` | mount-ns; `/proc/1` is self | **EQUIVALENT** | Host dirs absent from the rootfs; `/proc/1` is the guest's own pid-1 (PID ns synthesised by the sentry — **[verified live]**: only pid `1` visible). |
| 3 | `TestWriteOutsideWorkDir` | read-only root | **EQUIVALENT** | `root.readonly=true` in the OCI spec; writes succeed only on the `/work` bind and the tmpfs `/tmp`/`/dev`. Enforced by the sentry's VFS, not host mount flags. |
| 4 | `TestListRootDirectory` | minimal root contents | **EQUIVALENT** (informational) | Root listing reflects the purpose-built rootfs, not the host. Contents differ from nsjail's (a fuller toolchain tree) but expose only trusted distro files — by design (see `gvisor.go` ROOTFS STRATEGY). |
| 5 | `TestProcPidNamespaceIsolation` | PID namespace | **EQUIVALENT** | **[verified live]**: `getpid()==1`, only pid `1` under `/proc`. The sentry implements the PID namespace; there is no host PID table to enumerate even in principle. |
| 6 | `TestSeccompPtrace` | host seccomp KILL of `ptrace` | **N/A** (category error) | Guest `Seccomp: 0` **[verified live]** — there is no host seccomp filter on the guest. `ptrace` is serviced by the sentry against sentry-internal tasks; it cannot reach a host process. The deny-list's purpose (stop a host syscall) doesn't apply. |
| 7 | `TestSeccompUnshareNewuser` | host seccomp KILL of `unshare(CLONE_NEWUSER)` | **N/A** (category error) | **[verified live]**: `unshare(CLONE_NEWUSER)` **returns 0** under gVisor (vs SIGSYS-kill under nsjail). The new userns is in the **sentry's** user model; any caps gained act on emulated resources only. Allowing it is not a gap — see the §1 note. |
| 8 | `TestSeccompUmount` | host seccomp KILL of `umount2` | **N/A** (category error) | `mount`/`umount` operate on the sentry's VFS; there is no host mount namespace to restructure. Whatever the sentry permits affects only the guest's own emulated mounts. |
| 9 | `TestSeccompSetns` | host seccomp KILL of `setns` | **N/A** (category error) | No host namespace fds to join; the sentry owns the guest's namespaces. |
| 10 | `TestSeccompForkAllowed` | `fork` is allowed (negative control) | **EQUIVALENT** | `fork`/`clone` work under the sentry too — **[verified live]** (the §2.1 fork/thread tests create hundreds of tasks). The negative control's intent (normal process creation works) holds. |
| 16 | `TestSeccompCloneNewuserBlocked` | host seccomp closes the `clone`/`clone3` userns gap (Finding A) | **N/A** (category error) | Finding A was a *host-kernel* attack-surface concern: regaining `CAP_SYS_ADMIN` widens reachable host code paths. Under gVisor there is **no host syscall surface to widen** — the "capable" guest still routes every syscall through the sentry. **[verified live]**: `clone(CLONE_NEWUSER)`→`EPERM`, `unshare(CLONE_NEWUSER)`→`0`; neither matters. |
| 17 | `TestSeccompIoUringBlocked` | host seccomp denies `io_uring` (Finding B) | **N/A → arguably EQUIVALENT-by-default** | io_uring's danger under nsjail was (a) host kernel attack surface and (b) a channel running I/O on host kernel worker threads, invisible to host seccomp. Under gVisor the guest cannot reach host io_uring at all — gVisor does not expose a host io_uring ring to the guest; I/O is brokered through the sentry/gofer. Both halves of Finding B are structurally absent. |
| 11 | `TestMemoryBombResident{Py3,Java}` | cgroup `memory.max` resident OOM | **EQUIVALENT** (looser cap) | **[verified live]**: a process bomb is OOM-killed → `memory_exceeded` (exit 137). gVisor enforces the cgroup memory limit *and* bounds guest RAM internally. Caveat: the cap is **looser** (guest budget + 512 MB sentry headroom) — see G4. |
| 12 | `TestMemoryBombZeroPage` | resident vs virtual accounting boundary | **EQUIVALENT** (documents a boundary) | The zero-page boundary is a property of how resident memory is charged; the sentry's memory accounting reproduces the same "untouched private pages cost nothing" shape. Not re-run; not a breach either way. |
| 13 | `TestForkBombProcessLimit` | cgroup `pids.max` | **STILL RELEVANT — and the gap (G1)** | This is the one test whose concern is **weaker** under the current gVisor backend. `gvisorResources` deliberately omits `pids.limit` (POC §4/§6: a tight pids cap trips sentry startup), so there is **no task-count cap**. **[verified live]**: 600 guest threads created with no limit hit; a 400-process bomb is stopped only *indirectly*, by the memory cap. See §2.1. |
| 18 | `TestCpuExhaustionBound` | cgroup `cpu.max` bandwidth | **EQUIVALENT** | `gvisorResources` emits `cpu.quota/period`; the sentry honours it and derives the guest's visible CPU count from it (**[verified live]**: `os.cpu_count()==2` under a 2-core quota). Same throttle-not-kill model as nsjail. |
| 14 | `TestEffectiveCapabilities` | caps dropped despite `--privileged` | **EQUIVALENT** (different reason) | **[verified live]**: guest is uid 0 with `CapEff/CapBnd = 0`. But the *load-bearing* fact under gVisor is not the empty cap set — it is that caps are meaningless against the sentry (see the §1 note). Even a guest that regains caps via `unshare` reaches nothing. |
| 15 | `TestOutboundNetworkBlocked` | empty network namespace | **EQUIVALENT** | **[verified live]**: `connect(8.8.8.8:53)`→`ENETUNREACH` (101), no `/sys/class/net`. Enforced by `--network=none` (the sentry runs no netstack with an upstream), structurally stronger than "an empty netns" — there is no host network path at all. |
| 19 | `TestSingleFileWriteBounded` | `rlimit_fsize` single-file cap (Finding D) | **STILL RELEVANT — different shape** | The OCI spec sets `RLIMIT_NOFILE` but **not** `RLIMIT_FSIZE`; the single-file bound nsjail gets from its default `rlimit_fsize` is **not** replicated. Disk writes to `/work` are instead bounded by the bind-mount backing store; the **strace log** is a separate, larger disk concern — see G3. |
| 20 | `TestProcHostInfoLeak` | `/proc` host-info leak (Finding F) | **EQUIVALENT — this is the fix** | **[verified live]**: the synthesised `/proc` closes the leak (see G2). This is the test that most directly *passes better* under gVisor. |

### Grouping the N/A seccomp tests (6, 7, 8, 9, 16, 17)

These all share one explanation, so they need no per-test prose beyond the table:
**gVisor does not enforce isolation with a host seccomp deny-list on the guest.**
The guest runs with `Seccomp: 0` (**[verified live]** in `/proc/self/status`) because
it doesn't need a BPF filter — *every* guest syscall is unconditionally trapped and
serviced by the sentry. So the entire premise of Group 2 ("a named host syscall is
KILLed before it reaches the kernel") does not exist: there is no host kernel call to
kill. `ptrace`, `unshare(CLONE_NEWUSER)`, `umount`, `setns`, `clone3`, `io_uring_setup`
are each either handled by the sentry within the guest's emulated world or rejected by
the sentry's own logic — and in **neither** case do they reach the host. The nsjail
deny-list (`configs/seccomp.policy`) is **not consulted** by the gVisor backend at all.

### Note — why `unshare(CLONE_NEWUSER)` succeeding under gVisor is not a Finding-A regression

Under nsjail, Finding A was serious because regaining a full capability set widened
the **host kernel** attack surface (dozens of `CAP_SYS_ADMIN`-gated host code paths
became reachable by untrusted code on a shared kernel). The fix was to SIGSYS-kill all
three userns-creation primitives. Under gVisor the same `unshare(CLONE_NEWUSER)`
returns `0` and hands the guest "capabilities" — but those capabilities are evaluated
by the **sentry**, over the sentry's emulated resources, and every subsequent syscall
the now-"capable" guest issues is *still* serviced by the sentry. There is no host
kernel path to widen. This is the canonical category error: the dangerous thing under
nsjail (host caps) and the harmless thing under gVisor (sentry caps) look identical at
the syscall level but mean entirely different things because the boundary moved.

---

## 2. gVisor-specific threat model — the questions that actually matter here

The sentry **is** the boundary now, so the meaningful questions are about the sentry
and its host-side footprint, not about host kernel primitives.

### 2.1 Sentry / host-task resource exhaustion as DoS — **REAL GAP (G1), bounded**

**The setup.** `gvisorResources` (gvisor.go) translates `MemoryKB` → `memory.limit`
and `CPUMsPerSec` → `cpu.quota`, but **deliberately omits `spec.MaxProcesses`**
(no `pids.limit`). The inline rationale (and POC §4/§6) is that an OCI `pids` cap
trips a deterministic sentry-startup failure in this WSL2 environment — the sentry is
itself multi-threaded and forks a `umounter` helper at bring-up, so a guest-sized pids
cap starves the sentry's *own* tasks (`fork/exec … EAGAIN` / `newosproc`). So the guest
runs with **no task-count limit at all**, narrower than nsjail's per-guest `pids.max`
(64–100).

**What I tested (bounded, on the private instance — never `:8080`).**

- **No low task cap exists.** A py3 program spawning **600 threads** (each sleeping)
  reached the cap with **no limit hit** (`THREAD_REACHED_CAP started=600`). Under
  nsjail the equivalent fork bomb is stopped at 63–64 (`pids.max`). **[verified live]**
- **A process bomb is caught indirectly — by memory.** A 400-process `fork` bomb did
  **not** run to its cap; it was **OOM-killed → `memory_exceeded`** (exit 137) in
  ~2.2 s, because 400 python processes' combined resident footprint crossed the cgroup
  memory cap. **[verified live]** So *processes* (which have a footprint) are bounded
  by `memory.max`; *threads* (which share an address space) are the cheaper vector and
  are the ones that reached 600 freely.
- **Host-container stability held at the levels I tested.** Throughout the 600-thread
  and 400-process runs the container stayed `Up`, `/healthz` returned 200, **and a
  concurrent new sandbox started and ran cleanly** (`CONCURRENT_OK` / `CONCURRENT2_OK`)
  — i.e. the sentry-startup failure mode the POC saw did **not** trigger at these
  counts, and there was no cross-tenant impact. No `newosproc`/`EAGAIN`/`EOF` appeared
  in the container log. **[verified live]**

**What I did NOT do, on purpose.** I did **not** escalate threads × concurrency to find
the sentry's breaking point. The POC (§6) already established that sentry *bring-up*
fails under host thread pressure (`newosproc`/`EAGAIN`), and the task is explicit:
stop before anything that could destabilise the host. So the residual is **reasoned,
not pushed to failure**:

> A guest can create essentially unbounded **threads** (no pids cap, and threads evade
> the memory cap). Each guest OS thread is backed by sentry/host scheduling resources.
> At a high enough thread count × `NumCPU`-wide concurrency, the documented sentry
> bring-up failure (`newosproc`/`EAGAIN`) becomes reachable — which would manifest as
> **other tenants' sandboxes failing to start** (the "cannot read client sync file:
> EOF" the POC saw), an **availability/DoS** failure, **not** a host escape or an
> isolation breach.

**Severity & disposition.** DoS-class, host-availability, **not** an isolation breach,
and **narrower in blast radius than it sounds**: the cgroup memory cap stops process
bombs, the `cpu.max` quota throttles spinners, the wall-time context kills runaways,
and the container stayed stable at the levels tested. But it is the one place the
current gVisor backend is **genuinely weaker than nsjail** (which enforces `pids.max`
directly). The fix is the deferred one: set `pids.limit`, sized to **budget for the
sentry's own tasks** (POC §4 — guest `MaxProcesses` + a sentry-task headroom), and
retest on a real-Linux host where the KVM platform and larger pid headroom remove the
startup fragility. Until then this is a **documented known limitation of the gVisor
backend**, the direct analogue of nsjail's Finding C (CPU-exhaustion DoS).

### 2.2 `/proc`, `/sys`, env and process-visibility synthesis — **comprehensive (G2)**

Finding F flagged `/proc/{version,cpuinfo,meminfo,loadavg}`. I spot-checked those plus
the entries the audit did *not* check, to see whether any host fact leaks through a
different path. **[verified live]**, all from one py3 program under gVisor:

| Source | Observed under gVisor | Host fact leaked? |
|--------|----------------------|-------------------|
| `/proc/version` | `Linux version 4.19.0-gvisor #1 SMP … 2016` | **No** — synthetic, constant |
| `/proc/meminfo` `MemTotal` | `626688 kB` = `102400` (py3 budget) + `524288` (512 MB headroom) | **No** — reflects the cgroup limit, not host RAM |
| `/proc/loadavg` | `0.00 0.00 0.00 0/0 0` | **No** — co-tenant side channel **closed** |
| `/proc/stat` | all-zero cpu/intr lines | **No** — no host CPU-activity side channel |
| `/proc/uptime` | `0.05 0.00` | **No** — sandbox uptime, not host uptime |
| `/proc/self/maps` | sentry-managed (`[usertrap]`, synthetic dev ids) | **No** — no host ASLR/layout leak |
| `os.cpu_count()` / `/sys/devices/system/cpu/online` | `2` / `0-1` = the CPU quota | **No** — reflects `cpu.max`, not host 8 cores |
| **environment variables** | `{PATH, HOME, LANG}` only | **No** — the `runsc` invocation env is **not** inherited; `buildOCISpec` sets `Env` explicitly |
| `/sys/class/net` | absent | **No** — no network |
| `/sys/fs/cgroup/memory.max` | absent | **No** — guest can't read/tamper its own cgroup |
| **`/proc/cpuinfo`** | `model name: unknown`, **but** `cpu family: 6`, `model: 140`, full `flags` (avx512/sha_ni/vaes…), `cpu MHz: 2419.197` | **Partial — the one residual** |

**Conclusion:** the synthesis is comprehensive — kernel version, memory totals, load,
CPU activity, uptime, process table and **environment** all reflect the sandbox, not the
host, with **no new leak path** beyond the one already documented. The **only** residual
is `/proc/cpuinfo`: the human-readable model string is gone, but `cpu family`/`model`
(6/140 = Tiger Lake) and the full `flags` list still fingerprint the
**microarchitecture**, and `cpu MHz` tracks the real clock. This is the exact caveat the
POC (§3) and Finding F record — the sentry passes real CPUID through for correctness
(guest code that probes for AVX-512 must get a truthful answer). It is information
disclosure / fingerprinting, **no breach**, and unchanged from what was already known.
**Disposition: documented known limitation, no new finding.**

### 2.3 The strace audit log as a resource — **NEW, DoS-class (G3)**

Stage 3 enables `runsc --strace --debug --debug-log=<bundle>/runsc-%COMMAND%.log` by
default. Each traced guest syscall emits an ENTER and an EXIT line into the per-request
debug log, which lives in the per-request bundle dir (`os.MkdirTemp("")` → the
container's host-backed `/tmp`).

**The concern.** There is **no size cap** on this log, and a guest controls how many
syscalls it issues. A tight `openat` loop is the cleanest amplifier (`openat` is in the
`--strace-syscalls` scope).

**[verified live].** A py3 `open()/close()` loop:

- **~206 bytes of log per `openat`** (the E+X pair at `--debug` verbosity).
- **400k opens → a 78 MB debug log** in the bundle dir, observed mid-run by sampling
  inside the container, before the run hit `time_exceeded`.
- `--strace` roughly **doubled** wall time (200k opens: 13.9 s on vs 7.3 s off), which
  can itself push an otherwise-passing program into `time_exceeded`.

**Why it's distinct from the guest's own limits.** The log is written by `runsc`/the
sentry into the **bundle dir**, not the guest's `/work`. So the guest's file limits do
**not** bound it: there is no `RLIMIT_FSIZE` on the spec (see test 19 / G4), the guest's
`memory.max` doesn't charge host-side log disk, and `cpu.max` only throttles (it slows
the loop but the log still grows for the whole wall window). There is a **second**
amplification: after the run, `collectStraceEvents` reads the **entire** log and
accumulates one `tracer.Event` per matching line in the **goboxd host process** — a tight
openat loop becomes hundreds of thousands of `Event` structs in the server's memory,
outside any guest budget.

**Why it's bounded (and not a crisis).** The bundle dir is removed by `defer
os.RemoveAll(bundleDir)` when `Run` returns — **[verified live]**: after the 400k run,
the container's `/tmp` was back to 12 K. So the pressure is **transient per-run** and
**width-bounded by `NumCPU` concurrency**, exactly the shape of nsjail's Finding D
(disk-fill). It is not persistent and does not by itself crash the service.

**Severity & disposition.** DoS-class, availability, **not** a breach. It is a **new**
vector introduced by the Stage-3 audit trail (it does not exist for an nsjail run, whose
eBPF tracer writes to a ring buffer, not a per-syscall text file). Recommended
mitigations, in order of cost: (a) cap the debug-log size (rotate/truncate, or a
`--debug-log`-size guard) so a pathological run can't exceed a few MB; (b) bound
`parseStraceEvents`' accumulated events (it already caps line length — also cap count);
(c) the same container-side mitigation Finding D recommends (size-limited `/tmp`);
(d) `GOBOXD_GVISOR_STRACE=off` removes the vector entirely at the cost of the audit
trail. This is worth recording in `gvisor.go`'s Stage-3 notes as a known limitation.

### 2.4 Cross-mechanism consistency — same attack, both backends — **parity except G1 (G4)**

For concerns meaningful under **both** boundaries, I ran the same attack shape under
gVisor and compared to the documented nsjail behaviour:

| Concern | nsjail (post Findings A–C) | gVisor **[verified live]** | Asymmetry? |
|---------|----------------------------|----------------------------|------------|
| Exhaust memory beyond budget | OOM-kill at `memory.max` → `memory_exceeded` (test 11) | OOM-kill → `memory_exceeded`, exit 137 | **gVisor cap is looser** (guest budget + 512 MB headroom; `MemTotal` 626688 kB observed). Enforcement equivalent; *threshold* higher. |
| Outbound network | `ENETUNREACH` immediate (test 15) | `ENETUNREACH` (101) immediate | None — equivalent (gVisor arguably stronger: `--network=none`, no netstack upstream at all). |
| Spawn many tasks | `EAGAIN` at `pids.max` 64 → `runtime_error` (test 13) | **no limit**; 600 threads OK; process bomb caught only by memory cap | **gVisor weaker** — this is G1. |
| CPU spin | throttled by `cpu.max`, killed by wall → `time_exceeded` (test 18) | `cpu.max` honoured, wall context kills → `time_exceeded` | None — equivalent. |

**Net:** for memory and network, **neither backend is weaker** (gVisor equal-or-stronger
on isolation; looser only on the memory *threshold*). The single asymmetry where gVisor
is materially weaker is **task-count limiting (G1)**.

---

## 3. Known gVisor vulnerability class (literature, not new testing)

This is a documentation point about the **different risk profile**, not something to
reproduce.

- **nsjail's characteristic class is the kernel-syscall gap** — the shared host kernel
  is reachable, so the bugs that matter are (a) a dangerous syscall missing from the
  deny-list (Finding A's `clone`/`clone3`, Finding B's `io_uring`) and (b) a
  vulnerability in the real Linux kernel that sandboxed code can now reach with whatever
  surface the deny-list left open. The blast radius of such a bug is the **host kernel**.

- **gVisor's characteristic class is the sentry implementation bug.** Because the sentry
  is a from-scratch reimplementation of the kernel ABI in Go, the escape-relevant bugs
  are **logic or memory-safety errors inside the sentry itself**, in its
  syscall/VFS/netstack emulation; in the **gofer** (the 9P file-proxy process); or in the
  **platform** layer (systrap/KVM) that mediates guest↔sentry. gVisor's own security
  model documents this explicitly: defence-in-depth assumes the sentry can be
  compromised, so the sentry *itself* runs under a restrictive host seccomp profile and
  dropped privileges, precisely so that a sentry-escape bug still faces a second host
  barrier (see gVisor's published security model,
  `gvisor.dev/docs/architecture_guide/security/`, and the project's GitHub Security
  Advisories, `github.com/google/gvisor/security/advisories`). Writing the sentry in Go
  removes most of the C memory-corruption class but **not** logic bugs (incorrect
  permission checks, integer/path handling in the emulated VFS, netstack parsing).

- **The framing for Tracebox:** the two backends fail *differently*. For nsjail, "what
  kind of bug would matter" is a **kernel syscall gap** (the deny-list residual — exactly
  the `userfaultfd`/`perf_event_open`/`keyctl` items Finding B flags as still-uncovered).
  For gVisor it is a **sentry/gofer implementation bug** — a class Tracebox cannot patch
  (it lives in gVisor upstream) but *can* manage by pinning a maintained `runsc` release
  and tracking gVisor's advisory channel, the same way the nsjail side tracks kernel
  CVEs. Neither is strictly "more secure"; they trade a **large, shared, battle-tested**
  attack surface (the Linux kernel) for a **small, isolated, younger** one (the sentry).

---

## 4. Overall assessment — nsjail vs gVisor, head to head

For each security property that matters, how do **nsjail** (post Findings A–C fixes) and
the **gVisor backend** (Stages 1–3) compare. This table is the most useful single
artifact of this stage.

| Property | nsjail (post A–C) | gVisor (Stages 1–3) | Stronger |
|----------|-------------------|---------------------|----------|
| **Boundary** | host seccomp + namespaces + dropped caps; **shared host kernel** | the **sentry** (userspace kernel); guest syscalls never reach the host kernel | **gVisor** (smaller reachable real-kernel surface) |
| **`/proc` host-info leak (Finding F)** | host kernel version, CPU model, **live host loadavg** leak; known limitation | **synthesised** — version/meminfo/loadavg/stat/uptime/env all sandbox-scoped; only `cpuinfo` family/model/flags residual | **gVisor** (the reason this backend exists) |
| **Network isolation** | empty netns, `ENETUNREACH` **[test 15]** | `--network=none`, `ENETUNREACH` **[verified]**; no netstack upstream | **gVisor** (marginally; no host net path at all) |
| **Memory enforcement** | `memory.max` resident OOM, guest-exact budget **[test 11]** | OOM → `memory_exceeded` **[verified]**, but cap = budget **+ 512 MB sentry headroom** (looser threshold) | **nsjail** (tighter cap); enforcement parity |
| **CPU enforcement** | `cpu.max` bandwidth **[test 18]** | `cpu.max` honoured; also sizes guest CPU count **[verified]** | **tie** |
| **Process/task limiting** | `pids.max` 64–100, hard `EAGAIN` **[test 13]** | **none** (G1) — 600 threads OK; only memory/wall backstops | **nsjail** (gVisor's one real weakness) |
| **Process visibility** | PID ns, self only **[test 5]** | PID ns, self only **[verified]** | **tie** |
| **In-guest privilege (`--privileged`)** | caps dropped, empty `CapBnd` **[test 14]**; sound only because seccomp blocks userns | uid 0, empty caps **[verified]** — **and** caps are inert vs the sentry even if regained | **gVisor** (defence doesn't depend on a deny-list) |
| **Dangerous-syscall containment** | deny-list (residual: any unlisted syscall is allowed — `userfaultfd`, `perf_event_open`, …) | sentry mediates **all** syscalls; no deny-list residual | **gVisor** (allow-by-default-to-sentry vs deny-list) |
| **What a boundary bypass means** | a **kernel syscall gap** → host-kernel compromise (large shared surface) | a **sentry/gofer implementation bug** → still faces gVisor's own host seccomp 2nd layer | **gVisor** (defence-in-depth; smaller surface) |
| **Audit-trail side effect** | eBPF ring buffer (bounded) | **per-run strace text log, uncapped (G3)** — transient disk/parse DoS | **nsjail** |
| **Maturity / track record** | nsjail + Linux kernel: large, old, heavily audited | sentry: smaller, younger, Go-memory-safe but logic-bug-prone | judgement call |

**Bottom line.** gVisor is **stronger on the properties the threat model cares most
about** — it closes Finding F outright, shrinks the reachable real-kernel surface to a
small interception layer, makes the capability-drop defence robust (it no longer hinges
on a complete seccomp deny-list), and eliminates the deny-list-residual class entirely.
Its costs are narrow and **DoS-class, not isolation-class**: (1) **no task-count limit**
(G1) — the one place it is genuinely weaker than nsjail, fixable by wiring `pids.limit`
with sentry headroom; (2) a **looser memory threshold** (sentry headroom); (3) a **new
uncapped strace-log** vector (G3); and (4) a younger, differently-shaped trust base (the
sentry, §3). None of (1)–(3) is a breach, and all three have clear, proportionate fixes.

---

## A genuinely new test worth adding permanently?

Most of §1–§2 is either already covered (the synthesised-`/proc` payoff is escape test
20's exact subject, now passing *better*) or DoS-class behaviour that is awkward to
assert through the HTTP API. Two candidates are worth flagging, but **neither is added in
this stage** (the task prefers investigation over forced new test files):

1. **A gVisor `/proc`-synthesis assertion** — the positive mirror of test 20: under the
   gVisor backend, assert `/proc/version` is `*-gvisor`, `/proc/loadavg` is static-zero,
   and the environment is `{PATH,HOME,LANG}` only. This is genuinely valuable (it would
   *fail* if a future change regressed the synthesis that is the whole point of the
   backend) and is a clean black-box assertion. **Recommended** if/when the escape suite
   grows a gVisor-tagged variant — but it needs the suite parameterised by backend
   (`GOBOXD_RUNNER`), which is a suite-structure change out of scope here.

2. **A G3 strace-log-bound regression** — assert an `openat`-heavy run does not produce
   an oversized per-run log. Only meaningful **after** a size cap is implemented (§2.3);
   premature until then.

The G1 task-count gap is best closed in code (wire `pids.limit` with sentry headroom)
and covered by the existing unit tests in `internal/runner`, mirroring how the nsjail
`pids.max` fix was verified — rather than by a new black-box escape test that would only
confirm the *absence* of a limit today.

---

## Appendix — environment & method

- **Target:** a `tracebox:latest` container with `-e GOBOXD_RUNNER=gvisor` on a private
  port (8090; a second instance on 8091 with `GOBOXD_GVISOR_STRACE=off` for the strace
  A/B), `--privileged --cgroupns=host`, on Docker Desktop / WSL2 kernel
  `6.6.114.1-microsoft-standard-WSL2`, `runsc release-20260608.0`, systrap platform (no
  `/dev/kvm`). **Never** the shared `:8080`.
- **Method:** black-box `/run` HTTP submissions via `experiments/gvisor-poc/probe.py`
  (a temporary helper, not a committed test), the same path the escape suite uses.
- **Stability discipline:** the one potentially-destabilising check (§2.1 sentry
  exhaustion) was kept **bounded** (≤600 threads / ≤400 processes) with concurrent
  health + cross-tenant-startup probes, and **not** escalated to the sentry's failure
  point, per the task constraint. The container stayed healthy throughout; the residual
  high-count behaviour is reasoned from the POC, not forced.
