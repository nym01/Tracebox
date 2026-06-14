---
name: phase4-tracer-design
description: Phase 4 eBPF file-open tracer (internal/tracer) — architecture, cgroup-id-after-spawn filtering, v1 limitation
metadata:
  type: project
---

Phase 4 shipped a production eBPF file-open tracer in `internal/tracer/` (cilium/ebpf v0.19.0), wired into `/run`. Design:

- **One persistent attach** at goboxd startup (`tracer.Start` from `cmd/tracebox/main.go`): tracepoints `sys_enter_openat` + `sys_enter_openat2`, ring buffer, plus a `active_cgroups` BPF hash map. Tracefs is mounted by the tracer itself at startup (privileged). No per-request attach (avoids attach race/latency).
- **Filtering = cgroup-id, discovered after spawn.** `RunSpec.OnStart` callback (added to runner; called by `NsjailRunner.Run` after `cmd.Start`, joined via WaitGroup before return) → `Run.Attach(nsjailPID)` finds the sandboxed child via `/proc/<nsjailpid>/task/<nsjailpid>/children`, reads `/proc/<child>/cgroup`, stats `/sys/fs/cgroup<path>` (inode == `bpf_get_current_cgroup_id`), and Put()s it in `active_cgroups`. The kernel emits events only for registered cgroups; a userspace reader goroutine routes them by cgroup id to the run. Chosen over the POC's pidns filtering: child is in its cgroup before exec, no PID-ns translation, single hash lookup. Could not use *before-spawn* cgroup filtering because [[nsjail-owns-cgroup-creation]].
- **Output:** one JSON line per open `{"run_id","event":"file_open","syscall","path","timestamp"}` to stdout via `emitTraceEvents`, alongside the existing `emitRunLog` line. No storage/endpoint (Phase 5).
- **Build tags:** real impl is `//go:build linux` (`tracer_linux.go`, `proc_linux.go`); `tracer_other.go` is a no-op stub so non-Linux dev builds pass. All `Tracer`/`Run` methods are nil-safe (tracing-disabled = nil tracer). The `.bpf.c` uses `<linux/bpf.h>`+`<bpf/bpf_helpers.h>` and a hand-written tracepoint-ctx struct (no vmlinux.h); bpf2go output is gitignored and regenerated in the Dockerfile builder (clang/llvm/libbpf-dev/linux-libc-dev, `-I/usr/include/x86_64-linux-gnu`).

**v1 limitation (documented in doc.go):** opens between exec and discovery completing can be missed; race-free earliest-open capture is a v2 needing the nsjail cgroup-creation restructuring. **Overhead:** ~1–5ms/run (the `OnStart` /proc discovery join); event handling is off the request hot path. Verified: all 21 escape tests pass, go build/vet/test pass, py3 + cpp(build+run) emit correlated events.
