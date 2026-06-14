// Package tracer is goboxd's Phase 4 eBPF syscall monitor. It attaches, once for
// the lifetime of the process, to the openat(2)/openat2(2) and
// execve(2)/execveat(2) syscall-entry tracepoints and records every file a
// sandboxed run opens and every process it spawns, from outside the sandbox,
// attributed to the run by cgroup id.
//
// # Design (v1, minimal)
//
// One persistent attach, owned by goboxd startup (Start). A tracepoint, once
// attached, is global and always-on, so there is no per-request attach latency
// and no per-request attach race; the only per-request work is registering and
// unregistering a cgroup id in a kernel hash map (Run.Attach / Run.Close). The
// eBPF program emits an event only for tasks whose cgroup id is currently
// registered, so the rest of the host is filtered out in-kernel.
//
// # cgroup-id vs pidns filtering
//
// The exploratory POC (experiments/ebpf-poc) proved pidns filtering and noted
// cgroup-id filtering would be cleaner *if* the run's cgroup id were known
// before the child executes. Investigation showed it is not: nsjail creates the
// per-request cgroup itself (NSJAIL.<pid>, named after a PID nsjail only learns
// after fork — see external/nsjail/cgroup2.cc) and exposes no flag to use a
// pre-created cgroup. Pre-knowing the cgroup id would require patching vendored
// nsjail, risking the cgroup memory/pids/cpu limits the security audit fixes
// depend on. So v1 discovers the cgroup id *after* spawn (see proc_linux.go):
// it is still chosen over pidns because the child is placed in its cgroup before
// it execs, the id needs no PID-namespace translation, and the in-kernel match
// is a single hash lookup.
//
// # v1 limitation
//
// Because the cgroup id is discovered just after spawn (not before), the few
// events the child produces between exec and discovery completing — typically
// the dynamic-linker/libc/interpreter-startup opens, and the very first exec
// (the interpreter/compiler itself, spawned by nsjail before it is registered) —
// can be missed. Everything from discovery onward is captured: the nested execs
// a run makes (e.g. a bash script calling /bin/ls, or gcc invoking cc1/as/ld)
// land well after registration. Race-free capture of the earliest events is a v2
// concern that needs the nsjail cgroup-creation restructuring above.
//
// # exec argv capture
//
// Exec events carry the executable path plus a bounded prefix of argv (up to the
// first 8 arguments, each truncated to 63 bytes — see ARGV_MAX/ARG_LEN in
// trace.bpf.c). The caps keep the in-kernel copy loop within the BPF verifier's
// complexity budget and the event record a fixed size; the captured prefix is
// enough to recognise a spawn (the program and its leading flags/targets).
package tracer

import "time"

// Event is one syscall observed inside a traced run: a file open or a process
// spawn, distinguished by Kind.
type Event struct {
	Kind    string    // "file_open" or "exec"
	Syscall string    // "openat"/"openat2" for file_open; "execve"/"execveat" for exec
	Path    string    // the filename argument passed to the syscall
	Argv    []string  // captured argv prefix (exec only; nil for file_open)
	Time    time.Time // when user space observed the event
}
