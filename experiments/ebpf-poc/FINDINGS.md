# Phase 4 eBPF POC — Findings

**Scope:** prove eBPF file-open tracing works for a short-lived, PID-filtered
process in this WSL2/Docker environment. Exploratory only — nothing here is wired
into the goboxd API/runner/CLI/MCP.

**Verdict: it works.** We can observe every file a sandboxed process opens —
including the ones the sandbox *allows* — from outside the sandbox, at the
container level, filtered to a single run. One non-obvious gotcha (PID
namespaces) and one design constraint (attach must be persistent, not per-run)
shape the eventual integration; both are detailed below.

---

## 1. Environment

| Property | Value |
|----------|-------|
| Kernel | `6.6.114.1-microsoft-standard-WSL2` (Dec 2025) |
| BTF | present at `/sys/kernel/btf/vmlinux` → CO-RE works, no kernel headers needed at runtime |
| Container base | `debian:bookworm-slim` (runtime), `golang:1.25` (build) |
| Capabilities | `--privileged` ⇒ `CapEff: 000001ffffffffff` (all caps incl. CAP_BPF, CAP_PERFMON, CAP_SYS_ADMIN) |
| Tracepoints | `syscalls:sys_enter_openat` and `sys_enter_openat2` both present |

**Kernel is plenty modern for eBPF** — no kernel-version limitations were hit.
`openat2` exists alongside `openat`; the POC hooks both (glibc/python can route
through either).

### tracefs is not mounted by default

The `goboxd` container does **not** have tracefs/debugfs mounted. Both bpftrace
and `link.Tracepoint()` need it. Because the container is privileged this is a
one-liner to fix:

```sh
mount -t tracefs none /sys/kernel/tracing      # or: mount -t debugfs none /sys/kernel/debug
```

For integration this should be done once at container start (or added to the
Dockerfile/entrypoint), **not** per run.

---

## 2. Step 1 — bpftrace (manual)

bpftrace was **not** preinstalled but `apt-get install -y bpftrace` worked in the
running container (pulls libllvm/libbpfcc; installed `bpftrace v0.17.0`). No extra
capabilities beyond what `--privileged` already grants were needed.

### Tracing an actual nsjail-sandboxed run

A real run was fired through the live API (`POST /run`, py3) while bpftrace
watched openat system-wide, tagging each event with its host PID:

```sh
bpftrace -e 'tracepoint:syscalls:sys_enter_openat /comm=="python3"/ \
   { printf("pid=%d %s\n", pid, str(args->filename)); }'
```

The trace cleanly separated two processes by PID — the HTTP client and the
sandboxed run — and the **sandboxed** python3 (host pid 31868) showed its full
open trace, including the bits unique to the sandbox:

```
pid=31868 /lib/x86_64-linux-gnu/glibc-hwcaps/x86-64-v4/libm.so.6   <- minimal mount ns alters lib search
pid=31868 /lib/x86_64-linux-gnu/libc.so.6
pid=31868 /usr/lib/python3.11/encodings/__pycache__/utf_8.cpython-311.pyc
pid=31868 /etc/passwd
pid=31868 /tmp/goboxd-90034901/solution.py     <- the sandboxed source file
pid=31868 /proc/self/status                     <- what the submitted code actually read
```

**PID filtering worked as expected.** The two concurrent python3 processes got
distinct host PIDs and were trivially separable. `args->filename` (not
`args.filename` — bpftrace 0.17 syntax) gives the path; bpftrace truncates
strings to 64 bytes by default.

> Note on capturing the PID: `nsjail.go` does **not** currently expose the
> spawned child's PID. `cmd.Process.Pid` (nsjail.go:264) is the *nsjail wrapper*
> PID, not the inner python3 — the inner process lives in nsjail's own PID
> namespace as a descendant. For Step 1 we sidestepped this with a `comm` filter;
> for real integration the PID/namespace identification is the crux (see §4–5).

---

## 3. Step 2 — standalone Go + cilium/ebpf

`main.go` + `trace.bpf.c`, built with `cilium/ebpf` v0.19.0 and bpf2go. Toolchain
(documented in `README.md`): `golang:1.25` base + apt `clang llvm libbpf-dev
linux-libc-dev bpftool`. CGO not required — bpf2go embeds the compiled `.o`.

Two build snags worth recording:

1. **bpf2go `-type event` → "collect C types: not found".** A struct only used
   through a ringbuf pointer can be omitted from the object BTF. Fix: a dummy
   global `const struct event *unused_event __attribute__((unused));` forces
   emission.
2. **Debian's `golang` apt package is 1.19**, too old for cilium/ebpf (needs
   cmp/iter/maps/slices). Use the `golang:1.25` image.

### Sample output (PID-filtered, ns-aware)

Target python3 traced while a concurrent "noise" python3 ran. Only the target
appears:

```
tracing openat/openat2 for pid 41911 (Ctrl-C to stop)
23:00:48.123 pid=41911 openat /etc/hostname
23:00:48.123 pid=41911 openat /etc/os-release
23:00:48.123 pid=41911 openat /proc/self/status
23:00:48.126 pid=41911 openat /lib/x86_64-linux-gnu/libcrypto.so.3
23:00:48.138 pid=41911 openat /lib/x86_64-linux-gnu/libssl.so.3
23:00:48.144 pid=41911 openat /usr/lib/python3.13/re/__pycache__/__init__.cpython-313.pyc
...
```

The noise process's opens (`/etc/passwd`, `/etc/group`) never appear — in-kernel
filtering works.

---

## 4. The PID-namespace gotcha (most important finding)

The first PID-filter attempt captured **nothing**, even with the tracer attached
first. Root cause, measured directly:

```
bash (in container, --pid=host) says child pid = 40029
eBPF (bpf_get_current_pid_tgid >> 32) sees same process as pid = 41429
```

**The pid user space knows ≠ the pid eBPF reports.** Under Docker Desktop / WSL2
the container is *not* in the kernel's init PID namespace even with `--pid=host`,
so the global PID the eBPF helper returns differs from the namespaced PID that
`bash`/Go sees. A naive `pid == target` compare can never match.

This compounds in production: nsjail puts the sandboxed child in **its own** PID
namespace, so there are up to three layers — kernel-init (what eBPF sees) →
goboxd container ns (`cmd.Process.Pid`) → nsjail child ns.

**Fix used here:** identify the target's PID *namespace* by the `(dev, inode)` of
`/proc/<pid>/ns/pid` and compare the pid *within that namespace* using
`bpf_get_ns_current_pid_tgid(dev, ino, &nsinfo, size)`. That helper (kernel
≥5.7; ours is 6.6) returns non-zero for any task outside the target namespace, so
it also filters unrelated processes for free. This is the robust, namespace-
correct approach and it directly informs the integration design.

---

## 5. Race-condition assessment (for Phase 4 integration)

Runs are typically <1s (the trivial nsjail run measured `duration_ms=51`). The
question: can the tracer attach and capture before the process exits?

**The attach itself is not the race — if you do it right.** A tracepoint, once
attached, is global and always-on; it fires for every process from its first
`openat`. So the integration must keep **one persistent attach** for the lifetime
of `goboxd`, *not* attach per run. With a persistent attach there is no
per-run attach latency at all.

The real race is **identifying which events belong to a run** without missing the
early ones:

- **In-kernel filter written after spawn (what this POC does):** there's a small
  window between nsjail `exec`-ing the child and goboxd learning the child's
  pidns and writing the filter config. Early opens (the dynamic-linker/libc/
  interpreter-startup opens) can be missed. For a *file-access audit* that
  window matters.
- **Recommended: filter by cgroup, decided before the child runs.** nsjail
  already places each request in its own cgroup (the per-request `memory.max`
  cgroup — see the `--cgroup_mem_max` path and the `cgroup: host` note in
  `docker-compose.yml`). goboxd *creates* that cgroup, so its id is known
  **before** the child executes. An eBPF filter keyed on `bpf_get_current_cgroup_id()`
  against that pre-known id is **race-free** — every open from the very first one
  is attributed correctly, with no PID-namespace translation needed at all.

So: **PID-based filtering of a short-lived run is practically feasible**, and a
**cgroup-keyed** filter on a persistent attach is the stronger design — it
sidesteps both the attach race and the PID-namespace translation. The PID/ns path
proven here is the fallback if per-cgroup granularity ever proves insufficient.

### Open items for the integration design (Phase 4 main work — not this task)

- Persistent attach owned by goboxd startup; tracefs mounted at container start.
- Prefer cgroup-id filtering over pidns; have the runner surface the run's cgroup
  (and/or the nsjail child PID — not currently exposed).
- Ring-buffer drain cadence vs. burst of opens at interpreter startup (python
  emits ~30–100 opens in tens of ms); size the ringbuf and reader accordingly.
- Filename truncation (256 B here) and the `openat2` vs `openat` split.
- `seccomp.policy` already blocks `bpf(2)` *inside* the sandbox — consistent with
  running the monitor at container level, outside the jail (don't relax that).
