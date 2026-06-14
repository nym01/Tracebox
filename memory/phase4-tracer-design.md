---
name: phase4-tracer-design
description: Phase 4 eBPF tracer (internal/tracer) — file-open (4a) + exec/process-spawn (4b), cgroup-id-after-spawn filtering, v1 limitation
metadata:
  type: project
---

Phase 4 shipped a production eBPF file-open tracer in `internal/tracer/` (cilium/ebpf v0.19.0), wired into `/run`. Design:

- **One persistent attach** at goboxd startup (`tracer.Start` from `cmd/tracebox/main.go`): tracepoints `sys_enter_openat` + `sys_enter_openat2`, ring buffer, plus a `active_cgroups` BPF hash map. Tracefs is mounted by the tracer itself at startup (privileged). No per-request attach (avoids attach race/latency).
- **Filtering = cgroup-id, discovered after spawn.** `RunSpec.OnStart` callback (added to runner; called by `NsjailRunner.Run` after `cmd.Start`, joined via WaitGroup before return) → `Run.Attach(nsjailPID)` finds the sandboxed child via `/proc/<nsjailpid>/task/<nsjailpid>/children`, reads `/proc/<child>/cgroup`, stats `/sys/fs/cgroup<path>` (inode == `bpf_get_current_cgroup_id`), and Put()s it in `active_cgroups`. The kernel emits events only for registered cgroups; a userspace reader goroutine routes them by cgroup id to the run. Chosen over the POC's pidns filtering: child is in its cgroup before exec, no PID-ns translation, single hash lookup. Could not use *before-spawn* cgroup filtering because [[nsjail-owns-cgroup-creation]].
- **Output:** one JSON line per open `{"run_id","event":"file_open","syscall","path","timestamp"}` to stdout via `emitTraceEvents`, alongside the existing `emitRunLog` line. No storage/endpoint (Phase 5).
- **Build tags:** real impl is `//go:build linux` (`tracer_linux.go`, `proc_linux.go`); `tracer_other.go` is a no-op stub so non-Linux dev builds pass. All `Tracer`/`Run` methods are nil-safe (tracing-disabled = nil tracer). The `.bpf.c` uses `<linux/bpf.h>`+`<bpf/bpf_helpers.h>` and a hand-written tracepoint-ctx struct (no vmlinux.h); bpf2go output is gitignored and regenerated in the Dockerfile builder (clang/llvm/libbpf-dev/linux-libc-dev, `-I/usr/include/x86_64-linux-gnu`).

**v1 limitation (documented in doc.go):** opens between exec and discovery completing can be missed; race-free earliest-open capture is a v2 needing the nsjail cgroup-creation restructuring. **Overhead:** ~1–5ms/run (the `OnStart` /proc discovery join); event handling is off the request hot path. Verified: all 21 escape tests pass, go build/vet/test pass, py3 + cpp(build+run) emit correlated events.

## Phase 4b — exec / process-spawn tracing (extends the above)

Added `execve`/`execveat` syscall-entry tracepoints to the SAME persistent program, reusing the SAME `active_cgroups` filter and per-run registration (no change to `OnStart`/`Attach`/`Close`/runner). Design choices:
- **Second ring buffer** `exec_events` (1<<18) separate from `events` (opens) — keeps the open path byte-for-byte unchanged and stops an interpreter open-burst from evicting the larger exec records. Second reader goroutine `readExecLoop`, same Stop() teardown.
- **Captured = path + bounded argv.** New `struct exec_event` carries `filename[256]` plus `args[ARGV_MAX=8][ARG_LEN=64]` + `argc`; BPF copies up to 8 argv entries (each ≤63 bytes) via an unrolled `bpf_probe_read_user`/`_str` loop. Verifier accepted it on the WSL2 kernel (constant-offset writes into the reserved record). Fuller argv was deliberately capped to stay within verifier budget + fixed record size — documented in trace.bpf.c/doc.go.
- **Event model:** `tracer.Event` gained `Kind` ("file_open"|"exec") and `Argv []string`. `emitTraceEvents` switches on Kind → `execLog {run_id,event:"exec",syscall,path,argv,timestamp}` (argv omitempty) alongside the file_open lines.
- **bpf2go:** had to add `-type exec_event` to the `//go:generate` line (alongside `-type event`) or `traceExecEvent` isn't generated → Docker build fails. This bit once.
- **`GOBOXD_TRACER=off`** env added to main.go to skip `tracer.Start()` (for A/B + triage); only touches the tracer's own wiring.

**Which tracepoint fires:** only `execve` observed in practice (glibc/python/gcc/bash all route through it); `execveat` is attached but unseen — analogous to openat vs openat2 in 4a. **argv works great:** observed `g++ -o solution solution.cpp`, `cc1plus …` (capped at 8 args), and a deterministic nested `python3 -c "print('child ran')"`. **Overhead (clean A/B, same image, `GOBOXD_TRACER=off` as B):** p50 delta +1ms (py3), +12ms (cpp ~4% of its compile-dominated ~300ms), nested-exec test came out negative — all within host noise; per-exec BPF cost is sub-ms, dominated by the unchanged 4a OnStart join. Verified: 21/21 escape tests pass on rebuilt image, go build/vet/test pass (Win + GOOS=linux), /healthz+/readyz ok.
