//go:build ignore

// trace.bpf.c — EXPLORATORY POC (Phase 4 scoping), NOT wired into the API.
//
// Minimal eBPF program that hooks the openat(2) and openat2(2) syscall-entry
// tracepoints, filters by a single target PID supplied from user space, and
// emits (pid, timestamp, filename) records over a ring buffer. The point of the
// POC is to confirm we can observe *every* file-open a sandboxed process
// attempts — including ones the sandbox allows — from outside the sandbox.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

char LICENSE[] SEC("license") = "GPL";

#define FILENAME_MAX_LEN 256

struct event {
	__u32 pid;
	__u64 ts_ns;
	__u8  syscall; // 0 = openat, 1 = openat2
	char  filename[FILENAME_MAX_LEN];
};

// struct event is only ever touched through a ringbuf pointer, so clang may omit
// it from the object's BTF — which makes bpf2go's `-type event` fail with
// "collect C types: not found". A dummy global of the type forces BTF emission.
const struct event *unused_event __attribute__((unused));

// Ring buffer for events -> user space.
struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 1 << 20); // 1 MiB
} events SEC(".maps");

// Filter configuration written by user space (array slot 0) before attaching.
//
// PID-namespace caveat (the whole reason this isn't a plain pid compare): under
// Docker Desktop / WSL2 the monitor process and the kernel live in DIFFERENT pid
// namespaces, so the pid user space knows (e.g. cmd.Process.Pid) is NOT the
// global pid that bpf_get_current_pid_tgid() returns — and nsjail nests the
// sandboxed child in yet another pid namespace. So instead of comparing the
// global pid we identify the target's pid *namespace* by its nsfs (dev, inode)
// and compare the pid *within that namespace* via bpf_get_ns_current_pid_tgid().
// That helper also conveniently returns non-zero for any task not in the target
// namespace, so unrelated processes are filtered out for free.
struct filter_cfg {
	__u64 dev;   // st_dev of /proc/<pid>/ns/pid (nsfs device)
	__u64 ino;   // st_ino of /proc/<pid>/ns/pid (the pid-namespace inode)
	__u32 nspid; // target pid *as seen inside that namespace*; 0 = trace all
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct filter_cfg);
} filter SEC(".maps");

static __always_inline int handle(const char *user_fname, __u8 which)
{
	__u32 key = 0;
	struct filter_cfg *cfg = bpf_map_lookup_elem(&filter, &key);
	if (!cfg)
		return 0;

	__u32 pid;
	if (cfg->nspid != 0) {
		// Namespace-scoped match (see filter_cfg comment).
		struct bpf_pidns_info ns = {};
		if (bpf_get_ns_current_pid_tgid(cfg->dev, cfg->ino, &ns, sizeof(ns)) != 0)
			return 0; // current task is not in the target pid namespace
		if (ns.pid != cfg->nspid)
			return 0;
		pid = ns.pid;
	} else {
		// nspid == 0 means "trace everything" (firehose / smoke test).
		pid = bpf_get_current_pid_tgid() >> 32;
	}

	struct event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e)
		return 0;

	e->pid = pid;
	e->ts_ns = bpf_ktime_get_ns();
	e->syscall = which;
	// The filename pointer lives in user memory at syscall entry.
	bpf_probe_read_user_str(&e->filename, sizeof(e->filename), user_fname);

	bpf_ringbuf_submit(e, 0);
	return 0;
}

// Tracepoint argument layouts come from
// /sys/kernel/tracing/events/syscalls/sys_enter_openat{,2}/format. The first 8
// bytes are the common tracepoint header + the syscall nr; the filename pointer
// sits at a fixed offset we read via the generated context struct fields.
SEC("tracepoint/syscalls/sys_enter_openat")
int tp_openat(struct trace_event_raw_sys_enter *ctx)
{
	return handle((const char *)ctx->args[1], 0);
}

SEC("tracepoint/syscalls/sys_enter_openat2")
int tp_openat2(struct trace_event_raw_sys_enter *ctx)
{
	return handle((const char *)ctx->args[1], 1);
}
