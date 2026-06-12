# Tracebox — Threat Model

>
## What this protects against

Tracebox runs untrusted, possibly malicious source code (including AI-generated
code) submitted over HTTP, across 7 language runtimes (py3, cpp, c, bash, js,
java, verilog). The goal is to execute that code, capture its output, and return
a result — without the code being able to read, modify, or affect anything
outside its own sandboxed execution: the host filesystem, other processes, the
Go server itself, the Docker daemon, or the underlying machine.

A request flows: HTTP request → validation (filename, size, flag allow-list) →
the source is written to a per-request temp directory → nsjail wraps the
build/run commands → result is captured and returned. The sandbox boundary is
the nsjail-wrapped child process; everything outside that (the goboxd Go
process, the Docker container, the host) is meant to be unreachable from inside.

## The three isolation pillars

**Filesystem isolation (mount namespaces + bind mounts).** Each language runs in
its own mount namespace with a minimal, explicitly bind-mounted filesystem —
only the interpreter/compiler binary, its shared library dependencies, and a
writable per-request work directory are visible. This stops the sandboxed
process from reading or writing anything on the host filesystem that wasn't
explicitly mounted in — `/etc/passwd`, other users' files, the rest of the
container, are simply not present.

**seccomp (syscall filtering).** A deny-list blocks a specific set of syscalls
— `ptrace`, `bpf`, `mount`/`umount2`, `kexec_load`, `init_module`/`finit_module`/
`delete_module`, `reboot`, `swapon`/`swapoff`, `setns`, and `unshare` with
namespace-creating flags. These are the syscalls that could be used to escape
or undermine the other isolation mechanisms (e.g. `mount` could remount a
read-only bind mount as read-write; `setns` could join the host's namespaces;
`ptrace` could attach to and control another process). Everything else is
allowed, so normal program behavior (file I/O, memory allocation, process
creation for compilers, etc.) is unaffected.

**cgroup v2 memory limits.** Each language has a memory limit (from
`configs/languages.yaml`) enforced via `--cgroup_mem_max` with swap disabled,
so a process that allocates too much resident memory is OOM-killed rather than
being allowed to exhaust host memory or stall indefinitely.

**Why all three matter together:** a single mechanism alone has gaps. Filesystem
isolation alone wouldn't stop a process from using `setns`/`mount` to undo that
isolation from the inside — seccomp closes that. seccomp alone wouldn't stop a
resource-exhaustion attack (allocating memory isn't a "dangerous" syscall by
name) — cgroups close that. cgroups alone wouldn't stop a process that can see
the whole host filesystem from reading secrets — filesystem isolation closes
that. An attacker would need to find a gap in all three simultaneously, or a
flaw in the mechanism that enforces all three.

## The boundary: shared kernel

All three pillars — namespaces, seccomp, and cgroups — are kernel features,
enforced by one thing: **the Linux kernel itself**, shared between the
sandboxed process, the goboxd server, and the host.

This means the real boundary of Tracebox's security is **the correctness of the
kernel's implementation of these features**. If there is a bug in how the
kernel implements namespace isolation, seccomp filtering, or cgroup accounting
— a "container escape" class kernel vulnerability — all three pillars could be
bypassed at once, because they all rely on the same underlying enforcement
mechanism. Tracebox's defenses are configuration on top of the kernel; they
cannot be stronger than the kernel itself.

Concretely, this means: a sufficiently novel kernel exploit, run by sandboxed
code, could potentially escape the mount namespace, bypass the seccomp filter,
or escape the cgroup — regardless of how carefully the policy/mounts/limits are
configured, because the exploit operates beneath the layer where those policies
are enforced.

**Things outside the sandbox that are still part of the attack surface even if
the sandbox itself is perfect:**
- The container currently runs with `--privileged`, which grants a very broad
  set of capabilities to the container — broader than strictly required for
  nsjail's namespace/cgroup operations. This is itself a larger attack surface
  than a minimally-capable container.
- The goboxd Go process itself — a bug in request validation, YAML loading, or
  the runner code could be exploitable independent of the sandbox's correctness.
- The Docker daemon and container runtime — vulnerabilities here are outside
  Tracebox's control entirely.
- Network access from within the sandbox — not explicitly addressed by the
  three pillars above; the network namespace configuration needs review.

## Strengthening the boundary

**gVisor** is the next step up from a shared-kernel sandbox. It replaces the
kernel's syscall surface with a userspace reimplementation ("Sentry") — syscalls
from the sandboxed process are intercepted and handled by gVisor's own code
before (in most cases) ever reaching the real host kernel. This shrinks the
amount of real kernel code reachable by the sandboxed process to a small
interception layer, so a kernel vulnerability in the *real* kernel is much
harder for sandboxed code to reach.

**Firecracker** (or similar microVMs) go further: each sandboxed workload runs
in its own lightweight virtual machine with its own kernel, using hardware
virtualization (KVM). Even a full kernel exploit only compromises that one VM's
kernel, not the host's.

**Why this would be hard for Tracebox specifically:** Tracebox supports 7
languages with very different runtime requirements — Phase 1 showed this
clearly (Java's JVM needed its runtime image and module system, `dlopen`'d
libraries invisible to `ldd`, and a `/proc/self/exe`-based `JAVA_HOME`
derivation that required a symlink rather than a bind mount; Verilog's compiler
shells out via `system()`). Under gVisor, syscalls behave slightly differently
(gVisor doesn't implement 100% of Linux syscalls, and some have different
semantics) — each language runtime would need to be re-validated against
gVisor's syscall coverage, likely surfacing new compatibility issues similar to
the ones found in Phase 1, but against a different target. Under Firecracker,
the per-VM overhead and boot time would need to be weighed against the "many
short-lived sandboxes" usage pattern this service has.

## Known limitations

- **`--privileged`** is a broad capability grant used to give nsjail the
  permissions it needs for namespaces and cgroups. A more minimal set of
  `--cap-add` flags scoped to exactly what's needed would reduce the attack
  surface of the container itself, independent of the sandbox inside it.
- **Filesystem isolation, seccomp, and cgroup enforcement are all ultimately
  kernel-enforced** — see "The boundary: shared kernel" above. This is the
  fundamental limitation of this entire approach, not a bug to fix, but the
  reason gVisor/Firecracker exist as stronger alternatives.
- **The seccomp policy is a deny-list, not an allow-list.** This is a
  deliberate Phase 1 scope choice (broad compatibility across 7 very different
  language runtimes with minimal per-language tuning), but it means any
  dangerous syscall not explicitly anticipated and added to the deny-list is,
  by default, allowed.
- **Network namespace configuration** was not a focus of the three pillars
  above and needs explicit review — what network access, if any, does
  sandboxed code currently have?
- **Process-count limiting (`max_processes`) is enforced via a cgroup v2
  `pids.max` limit.** Every language in `configs/languages.yaml` carries a
  `max_processes` budget; it is parsed (`internal/language`), clamped on a
  per-request override (`internal/api` `effectiveLimits`), carried on
  `runner.RunSpec.MaxProcesses`, and emitted by `buildNsjailArgs` as
  `--cgroup_pids_max <max_processes>` — uniformly, for every language and for both
  the build and run steps. nsjail writes the value to the child cgroup's `pids.max`
  (`external/nsjail/cgroup2.cc`). This was a real gap in the first pass of the escape
  suite (the value was parsed but never reached the runner) and is now closed; the
  suite (Group 3, test 13) verifies it: a bounded fork bomb's `fork()` fails with
  `EAGAIN` at the configured count (c's run budget of 64) rather than running to its
  2000-process self-cap. Note the *semantics* differ from the memory limit: exceeding
  `pids.max` does **not** OOM-kill the process — the kernel fails the next
  `fork()`/`clone()` with `EAGAIN`, so the sandboxed program observes a failed syscall
  and typically exits non-zero, which the API reports as `runtime_error` (not a
  dedicated `process_limit_exceeded` status — an `EAGAIN`-from-`pids.max` is
  indistinguishable from any other non-zero exit, with no signal like OOM's 137 to key
  off). Blast-radius containment is independent and also holds: the sandbox's PID
  namespace is torn down on exit/timeout, so children cannot outlive the request or
  reach host processes. Normal compilation is unaffected — compiled-language build
  steps fork internal subprocesses (gcc/g++ → cc1/as/ld; iverilog → ivlpp/ivl), but
  the build budgets (100 for c/cpp/java, 50 for verilog) leave ample headroom.
- **The cgroup memory limit (`memory.max`) bounds RESIDENT memory only.** This is
  inherent to how the limit works, not a bug, but it has a sharp edge documented by
  the escape suite (Group 3, test 12): an allocation that never becomes resident —
  e.g. a large `MAP_PRIVATE` anonymous mapping that is only read, so its pages map
  the kernel's uncharged shared zero page — is not caught regardless of size (1 GiB
  vs a 100 MiB budget). Memory that is actually *used* (written) is charged and
  OOM-killed immediately, so this is not a resource-exhaustion bypass, but it does
  mean `memory.max` constrains physical footprint, not virtual address space or
  untouched allocations.
- **PID-namespace isolation relies on nsjail's default, not explicit config.** The
  escape suite (Group 1, test 5) verified the sandbox runs in its own PID namespace
  (the sandboxed process is pid 1; no host process is visible under `/proc`), which
  is positive isolation beyond the three named pillars. But it is inherited from
  nsjail's default behaviour (it clones a fresh PID namespace unless told otherwise)
  rather than an explicit `--clone_newpid` flag, so it would silently regress if
  `--disable_clone_newpid` were ever added to the argument builder.