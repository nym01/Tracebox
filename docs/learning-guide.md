# Tracebox — Learning Guide

A guided tour for **understanding this codebase deeply**, written to be re-read
months from now by the person who built it (you) — possibly under pressure (e.g.
in an interview). It is **not** a status/progress handoff; for "what's done and
what's left," see the memory files and the phase notes. This document answers a
different question: *how do I actually understand what this thing is and why it's
built the way it is?*

Read it in order the first time. After that, jump to the section you need.

> **Naming note (read this first, it will confuse you later otherwise):** the
> product is **Tracebox**. The Go module is `github.com/nym01/goboxd` and the
> server binary's internal name is **goboxd**. So "goboxd" in the code and
> "Tracebox" in the docs are the same thing. Temp dirs are `goboxd-*`, env vars
> are `GOBOXD_*`.

---

## 1. Start here — the 3 things to read first

If you read nothing else, read these, in this order. They are the spine of the
whole system.

| Order | File | What it gives you |
|-------|------|-------------------|
| 1 | `internal/runner/runner.go` | The **`Runner` interface**, `RunSpec` (what goes in) and `RunResult` (what comes out). ~85 lines, mostly comments. This is the contract every sandbox backend implements. |
| 2 | `internal/api/handlers.go` → the `run()` function | The **`POST /run` flow end to end**: parse → validate → build (compiled langs) → run each test → map results → log + persist → respond. |
| 3 | `internal/runner/nsjail.go` → `buildNsjailArgs()` | How the **default real sandbox** is actually assembled — every security flag (seccomp, cgroups, mount namespace) is added here, with a comment explaining why. |

### The mental model in one paragraph

A request comes in to `POST /run` with source code and test cases. The handler
writes the source to a fresh temp dir, then asks a **`Runner`** to execute it —
first a build step for compiled languages, then once per test case. The `Runner`
is an interface; in production it's `NsjailRunner` (or `GvisorRunner`), which
wraps the command in a sandbox before executing it. Results come back as a
uniform `RunResult` regardless of which sandbox ran them. The handler maps that
to a status, emits a structured log line, persists an audit record, and responds.
**Everything else in the codebase exists to make that loop safe.**

### The Runner contract (memorize the shape)

```
RunSpec  →  Runner.Run(ctx, spec)  →  RunResult, error
```

- **`RunSpec`** = what to run + the limits to run it under: `Cmd`, `Args`,
  `Stdin`, `WorkDir`, `WallTimeSec`, `MemoryKB`, `MaxProcesses`, `CPUMsPerSec`,
  and an `OnStart` callback (lets the eBPF tracer discover the child).
- **`RunResult`** = what happened: `Stdout`, `Stderr`, `DurationMs`,
  `MemoryPeakKB`, `ExitCode`, `TimedOut`, `MemoryExceeded`, and `TraceEvents`
  (the gVisor path's audit events).
- **Three implementations**, all returning the identical shape so callers never
  change:
  - `SubprocessRunner` — no sandbox, just runs the command. The default; used
    in dev/tests and when no sandbox is selected.
  - `NsjailRunner` — wraps the command in nsjail (namespaces + seccomp +
    cgroups). The production sandbox.
  - `GvisorRunner` — runs the command under gVisor's `runsc`. The
    stronger-isolation alternative (`GOBOXD_RUNNER=gvisor`).

The selection happens once at startup in `cmd/tracebox/main.go` →
`selectRunner()`, keyed off `GOBOXD_RUNNER`.

---

## 2. Core concepts, explained simply

For each big idea: **what it is**, and **why this project needs it**. Plain
language, no assumed recall.

### seccomp (syscall filtering)

- **What:** a Linux feature that lets you install a filter deciding which
  *system calls* a process may make. A program talks to the kernel only through
  syscalls (open a file, create a process, allocate memory…), so filtering them
  filters what the program can ask the kernel to do.
- **Why we need it:** untrusted code shares the host kernel. A few syscalls are
  pure escape/tamper tools (`ptrace`, `mount`, `bpf`, `kexec_load`, namespace
  creation). We block those so the code can't reach them, while leaving normal
  syscalls (read/write/mmap/fork) alone.
- **In this codebase:** `configs/seccomp.policy`, written in **kafel** (nsjail's
  policy language), compiled to a seccomp-BPF filter and applied to every
  language uniformly. It's a **deny-list**: allow by default, kill a named set.
  Two actions: `KILL` (task dies instantly with SIGSYS) and `ERRNO(38)` (syscall
  returns "not implemented," so runtimes that *probe* a feature fall back
  gracefully instead of crashing).

### cgroups v2 (resource limits)

- **What:** the kernel's mechanism for capping a group of processes' resource
  use — memory, CPU, number of processes.
- **Why we need it:** isolation stops a program from *escaping*, but not from
  *exhausting* the host. Without limits, an infinite loop pins a core, a memory
  bomb eats all RAM, a fork bomb spawns until the box dies. cgroups bound each
  run so abuse is contained to that run.
- **In this codebase:** three limits, all per-request, all written by nsjail onto
  the child's cgroup (`buildNsjailArgs`):
  - `memory.max` (+ `memory.swap.max=0`) — exceed it → **OOM-killed** (SIGKILL).
  - `pids.max` — exceed it → `fork()` fails with **EAGAIN** (not killed).
  - `cpu.max` — exceed it → **throttled** (not killed; the wall timer ends it).
  - The key distinction: memory **kills**, pids and cpu **don't**.

### namespaces (isolation)

- **What:** the kernel feature that gives a process its own private view of some
  global resource. A *mount* namespace = its own filesystem tree; a *PID*
  namespace = its own process numbering (it sees itself as PID 1, can't see host
  processes); a *network* namespace = its own (here: empty) network stack; a
  *user* namespace = its own user/capability mapping.
- **Why we need it:** they're how "the program only sees a tiny room" is actually
  enforced. Mount namespace → can't see host files. PID namespace → can't see or
  signal other processes. Network namespace → no internet.
- **In this codebase:** nsjail sets these up *before* the seccomp filter and
  *before* exec. The mount namespace is built per-language by bind-mounting only
  the files that language needs read-only (the big `resolve*Mounts` functions in
  `nsjail.go`) plus the writable per-request work dir. The network namespace is
  empty (only loopback), so outbound connects fail with `ENETUNREACH`.
- **The subtlety (Finding A):** user-namespace *creation* is itself dangerous,
  because a new user namespace hands you a full capability set inside it. That's
  why creating one is blocked by seccomp. See §3.

### eBPF tracing (the audit trail, nsjail path)

- **What:** eBPF lets you run small sandboxed programs *inside the kernel*,
  attached to events. Here, attached to syscall-entry **tracepoints** for
  file-opens, exec, and connect.
- **Why we need it:** we want an audit trail of what each run *did* — which files
  it opened, what it tried to exec, where it tried to connect — observed from
  **outside** the sandbox (so the sandboxed code can't tamper with its own log).
- **In this codebase:** `internal/tracer/`. One persistent attach for the whole
  process lifetime (`Start`); per-request it just registers/unregisters the run's
  **cgroup id** in a kernel hash map, and the in-kernel program only emits events
  for registered cgroups. The `OnStart` callback in `RunSpec` is how the tracer
  discovers the child's cgroup id just after spawn. **Observability, not
  enforcement** — it records the connect attempt even though the empty network
  namespace makes it fail.

### gVisor's sentry model (the alternative backend)

- **What:** gVisor (`runsc`) is a **userspace re-implementation of the Linux
  kernel**, written in Go. The "sentry" is that userspace kernel. Guest syscalls
  are trapped and serviced by the sentry — they **never reach the host kernel
  directly**.
- **Why we need it (optionally):** with nsjail, the real boundary is the shared
  host kernel; any unblocked dangerous syscall is a risk (the deny-list residual).
  gVisor moves the boundary to the sentry, shrinking the reachable real-kernel
  surface to a tiny interception layer. Concretely it **closes Finding F** (the
  `/proc` host-info leak) outright, because the sentry presents a *synthesised*
  `/proc` instead of the host's.
- **In this codebase:** `internal/runner/gvisor.go`. One read-only **rootfs per
  language** baked into the image; each request writes a tiny OCI bundle pointing
  at that rootfs. Because the guest's syscalls don't hit the host kernel, the eBPF
  tracer sees nothing — so the gVisor path instead parses `runsc --strace` output
  into `RunResult.TraceEvents` (`gvisor_strace.go`).
- **The trade (read `docs/gvisor-security-assessment.md`):** gVisor is stronger on
  almost everything, but it trades a **large, old, battle-tested** attack surface
  (the Linux kernel) for a **small, young** one (the sentry). Its one real
  weakness vs nsjail is **G1** — no task-count limit (see §4).

---

## 3. A worked example — Finding A, told as a story

This is the headline security finding. Learn to re-tell it without re-reading
`docs/security-audit-findings.md`. The full version is there; this is the
memorable version.

### The original setup

The threat model leaned on a claim: *"the sandboxed process is root-without-power.
Even though it runs as uid 0, its capability bounding set (`CapBnd`) is empty, so
it can never **regain** a capability."* This was presented as a load-bearing
security property — a layer you could rely on.

To make that true, the seccomp policy blocked creating a new **user namespace**,
because a new user namespace hands you a full capability set inside it. The policy
blocked it on the `unshare` syscall:

```
unshare { (unshare_flags & (CLONE_NEWUSER | CLONE_NEWNS)) != 0 }
```

### The gap

`unshare` is not the only way to create a user namespace. **`clone`** and
**`clone3`** create exactly the same namespaces — `unshare(flags)` and
`clone(flags|SIGCHLD, ...)` are interchangeable for this purpose. The policy
closed the `unshare` door and left the `clone`/`clone3` doors wide open. There
was no rule for either.

### How it was found

Adversarial review, with the lens *"I have arbitrary code execution inside the
sandbox — what can I reach that the 15 escape tests don't cover?"* A short C
program calling `clone(child_fn, stack, CLONE_NEWUSER | SIGCHLD, NULL)` was run
against the live sandbox. The child read its own `/proc/self/status`:

```
CapBnd: 000001ffffffffff      <-- full bounding set ("empty" per the threat model)
```

`000001ffffffffff` is **all 40 capabilities**, including `CAP_SYS_ADMIN`. The
child could then `sethostname()` (which returns `EPERM` in the base sandbox) and
it **succeeded**. The "capability-less" property was simply false as written.

### Why the fix was non-obvious

You can't just block all three syscalls the same way:

- **`clone`** *can* be filtered like `unshare`, because its flags are a direct
  register argument seccomp can inspect. But you must filter only the namespace
  flags — a blanket kill would break **all threading**, because every
  multi-threaded runtime (the JVM, V8/Node) creates threads via
  `pthread_create → clone`.
- **`clone3`** *cannot* be argument-filtered at all: it passes its flags inside a
  `struct clone_args` **behind a pointer**, and seccomp cannot dereference
  pointers. So you can't inspect `CLONE_NEWUSER`. And you can't just `KILL`
  `clone3` either, because glibc ≥ 2.34 calls `clone3` *first* for ordinary
  `fork`/`pthread_create` and only falls back to `clone` on failure — a hard kill
  would be a guaranteed regression on modern runtimes.

### The fix

Two rules in `configs/seccomp.policy` (this is the standard container-runtime
approach — it's how Docker's default profile handles `clone3`):

1. **`clone`** — argument-filtered on the namespace flags, exactly like `unshare`:
   `clone { (clone_flags & (CLONE_NEWUSER | CLONE_NEWNS)) != 0 }` → **KILL**.
   Ordinary thread/process creation never sets those flags, so it's untouched.
2. **`clone3`** — returned **`ENOSYS`** ("not implemented"). This makes glibc
   transparently and permanently fall back to the classic `clone` syscall (its
   built-in old-kernel path) — which is now filtered. An attacker calling
   `clone3(CLONE_NEWUSER)` directly just gets `ENOSYS` and creates no namespace.

So: `clone3` → `ENOSYS` → glibc retries with `clone` → KILLed if it asks for a
user namespace, allowed otherwise. Every path to a new user/mount namespace is
closed; nothing legitimate breaks. Verified by escape test 16 plus re-running all
7 languages including multi-threaded Java.

### The one-sentence version

> *The "sandbox is capability-less" guarantee rested on blocking user-namespace
> creation, but it only blocked `unshare` — `clone`/`clone3` create the same
> namespace and were unfiltered, so code could regain `CAP_SYS_ADMIN`; the fix
> arg-filters `clone` and `ENOSYS`-denies `clone3` so glibc falls back to the
> filtered `clone`.*

---

## 4. Interview-style questions & answers

Plain-language, 2–4 sentences each, with a pointer for going deeper.

### Why `ENOSYS` for `clone3` but `SIGSYS`/`KILL` for `clone` with namespace flags?

`clone`'s flags are a register argument, so seccomp can inspect them and KILL only
the dangerous calls (namespace creation) while allowing normal threading.
`clone3` passes its flags behind a pointer that seccomp can't dereference, so it
can't tell a dangerous call from a benign one — and a blanket KILL would break
glibc ≥ 2.34, which uses `clone3` for ordinary `fork`/`pthread_create`. `ENOSYS`
threads the needle: it denies the capability while making glibc fall back to the
(filtered) classic `clone`. → `configs/seccomp.policy`, §3 above.

### Why is exit 137 unambiguous for OOM but not for timeout?

137 = `128 + 9` = killed by SIGKILL. **Two** things SIGKILL the child: the cgroup
OOM killer (memory exceeded) *and* nsjail's own `--time_limit` enforcement at the
wall deadline. So 137 alone can't distinguish them. The code resolves it with the
separately-computed `timedOut` flag: 137 is attributed to memory **only when the
run did not time out** and a memory limit was actually in force. → `oomKilled()`
in `internal/runner/nsjail.go`.

### Why does `MAP_PRIVATE` allocation walk free under the memory cgroup but `MAP_SHARED` gets charged?

cgroup `memory.max` charges **resident** memory — pages actually backed by RAM. A
fresh `MAP_PRIVATE` anonymous mapping is copy-on-write against the shared zero
page: until you *write* to a page, it costs no resident memory, so allocating huge
private regions you never touch doesn't trip the limit. `MAP_SHARED` pages are
backed immediately, so they're charged on allocation. This is why the test suite
distinguishes "resident" from "virtual" memory bombs. → escape tests 11/12, and
the `--rlimit_as max` comment in `nsjail.go` (we deliberately *don't* cap virtual
memory).

### Why does the JVM defeat `rlimit_as` but not cgroup `memory.max`?

`rlimit_as` caps **virtual** address space. Managed runtimes (the JVM, V8) reserve
enormous virtual regions up front regardless of real use — the JVM's heap reserve,
V8's code range — so any `rlimit_as` tight enough to bound real memory aborts them
before user code runs ("Could not reserve enough space for object heap"). So we
lift `rlimit_as` to max and cap **resident** memory via cgroup `memory.max`
instead: the runtime can reserve all the virtual space it wants, but the instant
its *real* footprint crosses the budget it's OOM-killed. → the `--rlimit_as max`
and `--cgroup_mem_max` comments in `buildNsjailArgs`, `nsjail.go`.

### What's the difference between nsjail's and gVisor's security boundaries, in one sentence each?

- **nsjail:** the boundary is the **host kernel** itself, fenced off with host
  seccomp + namespaces + dropped capabilities — strong, but every unblocked
  syscall is reachable real-kernel surface (the deny-list residual).
- **gVisor:** the boundary is the **sentry**, a userspace re-implementation of the
  kernel, so guest syscalls are serviced in userspace and never reach the host
  kernel directly — a much smaller reachable real-kernel surface, at the cost of
  trusting a younger codebase.
- → `docs/gvisor-security-assessment.md` §4 (the head-to-head table).

### What's G1 (gVisor's pids gap) and why wasn't it fixed?

G1 is the one place the gVisor backend is **genuinely weaker than nsjail**: it
sets **no task-count limit** (`pids.limit`) on the guest, so a guest can spawn
essentially unbounded threads (600+ observed, vs nsjail's `pids.max` of ~64). It
was left unfixed because, in the WSL2 dev environment, an OCI `pids` cap trips a
deterministic **sentry-startup failure** — the sentry is itself multi-threaded and
a guest-sized cap starves its own tasks. It's contained *indirectly* (process
bombs are caught by the memory cap; spinners by `cpu.max` + the wall timer) and is
**DoS-class, not an isolation breach**. The real fix is to set `pids.limit` sized
to include sentry headroom and retest on a real-Linux host. → `gvisor-security-
assessment.md` §2.1.

---

## 5. Glossary

One or two sentences each, no assumed prior knowledge.

| Term | Meaning |
|------|---------|
| **syscall** | A request from a program to the kernel (open a file, fork, allocate memory). The only way a program reaches the OS. |
| **seccomp** | A kernel filter that decides which syscalls a process may make. The basis of our syscall blocking. |
| **kafel** | The policy language nsjail uses to express a seccomp filter (`configs/seccomp.policy`). Compiled to seccomp-BPF. |
| **SIGSYS / SIGKILL** | Signals that terminate a process. A seccomp `KILL` delivers SIGSYS (→ exit 159); the OOM killer and wall-timeout deliver SIGKILL (→ exit 137). |
| **ENOSYS** | The "function not implemented" error. Used to deny a syscall *gracefully* so a runtime probing for a feature falls back instead of crashing. |
| **capability** | A fine-grained slice of root's power (e.g. `CAP_SYS_ADMIN`). The sandbox drops them all; Finding A was about *regaining* them. |
| **cgroup (v2)** | Kernel mechanism to cap a process group's resources. We use `memory.max`, `pids.max`, `cpu.max`. |
| **namespace** | A private per-process view of a global resource: mount (filesystem), PID (processes), network, user. The basis of isolation. |
| **user namespace** | A namespace that remaps users/capabilities; *creating* one grants a full capability set inside it — hence dangerous (Finding A). |
| **mount namespace** | A private filesystem tree. We populate it with only the files a language needs (read-only) plus a writable work dir. |
| **nsjail** | The process-isolation tool (vendored in `external/nsjail`) that sets up namespaces + seccomp + cgroups, then execs the command. Our default sandbox. |
| **gVisor / runsc** | A userspace kernel re-implementation that services guest syscalls so they never hit the host kernel. The alternative backend. |
| **sentry** | gVisor's userspace kernel — the actual security boundary under the gVisor backend. |
| **gofer** | gVisor's separate file-proxy process (9P) that brokers the sentry's file access. Part of gVisor's trust base. |
| **rootfs** | A populated filesystem tree a sandbox runs against. gVisor uses one read-only rootfs per language (baked into the image). |
| **OCI bundle** | The little `config.json` + root spec that tells `runsc` what to run. Written fresh per request by `GvisorRunner`. |
| **eBPF** | A way to run small sandboxed programs inside the kernel, attached to events. Powers our out-of-sandbox audit tracer. |
| **tracepoint** | A stable hook point in the kernel (e.g. syscall entry) that an eBPF program can attach to. We attach to open/exec/connect entries. |
| **ring buffer** | A bounded kernel→userspace channel the eBPF tracer uses to stream events out. "Bounded" is why it's not a DoS vector (contrast G3's strace log). |
| **strace** | Per-syscall tracing. `runsc --strace` is how the gVisor path gets an audit trail (the eBPF tracer can't see sentry-serviced syscalls). |
| **OOM killer** | The kernel subsystem that SIGKILLs a process exceeding its memory cgroup. Produces exit 137 → `memory_exceeded`. |
| **RunSpec / RunResult** | The input/output structs of the `Runner` interface. Memorize their fields (§1). |
| **goboxd** | The internal name of the Tracebox server binary and Go module. Same thing as "Tracebox." |

---

## 6. Map of the codebase

A tour for navigating the repo after time away. Top-level first, then the
important packages.

### Top level

| Path | What lives here |
|------|-----------------|
| `cmd/` | The three binaries (entry points). |
| `internal/` | All the real logic (Go's `internal/` = not importable outside this module). |
| `configs/` | `languages.yaml` (per-language commands + limits) and `seccomp.policy` (the kafel deny-list). |
| `escapetests/` | The 21 adversarial escape tests (`go test -tags escapetests`). Each is "try to break out, assert it fails." |
| `experiments/` | Throwaway POCs that de-risked the real work: `ebpf-poc/` (proved eBPF filtering) and `gvisor-poc/` (proved gVisor synthesises `/proc`). Not production code. |
| `external/nsjail` | Vendored nsjail (a git submodule). We read its source (e.g. `cgroup2.cc`) to confirm flag behaviour rather than guessing. |
| `web/` | The React + Monaco frontend (`/run` client). |
| `docs/` | All the documents — see below. |
| `Dockerfile`, `docker-compose.yml` | How the image (with nsjail, runsc, the rootfs trees) is built and run (`--privileged --cgroupns=host`). |

### `cmd/` — the three binaries

| Path | What it is |
|------|-----------|
| `cmd/tracebox/` | **The server** (goboxd). `main.go` wires everything: `selectRunner()` (subprocess/nsjail/gvisor), the tracer, the store, the HTTP routes. Start here for "how does the process boot." |
| `cmd/tracebox-cli/` | The **CLI** — `start`/`stop` subcommands that drive the container, `--strict` selects the gVisor backend. `sandbox.go` is the container orchestration. |
| `cmd/tracebox-mcp/` | The **MCP server** — exposes `tracebox_run` as a tool so an AI agent can run code in the sandbox. |

### `internal/` — the logic (in dependency order)

| Package | Responsibility | Key files |
|---------|---------------|-----------|
| `internal/api` | HTTP layer: the `/run` flow, request validation, limit clamping, concurrency cap, result→status mapping, persistence, the `/runs` audit endpoints. | `handlers.go` (the `run()` flow), `validate.go`, `runs.go`, `concurrency.go` |
| `internal/runner` | **The sandbox abstraction.** The `Runner` interface and its three implementations. | `runner.go` (interface), `subprocess.go`, `nsjail.go`, `gvisor.go`, `gvisor_strace.go` |
| `internal/tracer` | The eBPF audit tracer (Linux only). Attaches once, filters by cgroup id, emits file_open/exec/connect events. | `doc.go` (read this), `trace.bpf.c`, `tracer_linux.go`, `proc_linux.go` |
| `internal/language` | The language registry — loads `configs/languages.yaml`, exposes per-language commands, filenames, and limits. | `language.go`, `loader.go` |
| `internal/store` | SQLite persistence of runs + their trace events (the audit trail). Nil-safe so the handler can call it unconditionally. | `store.go` |
| `internal/status` | Maps raw run outcomes to API statuses (`accepted`, `runtime_error`, `time_exceeded`, `memory_exceeded`, `build_failed`, …) and computes the top-level status. | `status.go` |
| `internal/compare` | Compares actual stdout to expected stdout for a test case. | `compare.go` |

### `docs/` — which document answers what

| Document | Read it when you want… |
|----------|------------------------|
| `docs/learning-guide.md` | **This file** — how to understand the project. |
| `docs/security.md` | The plain-language threat model: the three layers, the boundary, known weaknesses. The "explain it to anyone" version. |
| `docs/security-audit-findings.md` | The nsjail red-team memo: Findings A–G in full detail, each with gap → proof → fix. Finding A (§3 here) is the headline. |
| `docs/gvisor-security-assessment.md` | The gVisor red-team memo: why the boundary moved, why most nsjail tests are "category errors" under gVisor, G1/G3, and the head-to-head table (§4 of that doc is the single best artifact). |
| `docs/escape-tests.md` | What each of the 21 escape tests probes and why. |
| `docs/decisions.md` | The numbered design decisions (D1, D2, …) — *why* a thing was done a particular way. |
| `docs/phase5-notes.md`, `docs/ai/` | Phase-by-phase build notes and AI-collaboration records (ADRs, patterns, plan evolution). The "how this got built over time" trail. |

### A few load-bearing details worth remembering

- **Limits can only be tightened, never loosened.** A request may override a
  language's limits, but `effectiveLimits()` caps every override at the language
  default (`internal/api/handlers.go`).
- **The seccomp policy is uniform.** Every language gets the *same*
  `configs/seccomp.policy`; only the filesystem mounts differ per language.
- **Build and run are separate sandbox invocations** that hand off the compiled
  artifact through the shared work dir — which is why that dir must be a
  host-backed bind mount (this is also why Finding D's disk quota is hard).
- **The audit trail is runner-agnostic at the storage layer.** nsjail events come
  from the eBPF tracer; gVisor events come from `runsc --strace`; both are merged
  into one `[]tracer.Event` by `combineTraceEvents()` and stored the same way, so
  `/runs/{id}` doesn't care which backend ran.
