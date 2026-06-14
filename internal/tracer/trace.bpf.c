//go:build ignore

// trace.bpf.c — Phase 4 file-open tracer (production, wired into goboxd).
//
// Hooks the openat(2) and openat2(2) syscall-entry tracepoints and emits
// (cgroup id, timestamp, filename) records over a ring buffer for every open
// made by a task whose cgroup id is registered in the active_cgroups map. User
// space (internal/tracer) registers a run's cgroup id when the run starts and
// removes it when the run ends, so only the sandboxed children of in-flight
// /run requests produce events — the rest of the host is filtered out in-kernel.
//
// Why cgroup-id filtering (not the POC's pidns filtering): every nsjail run is
// placed in its own cgroup (NSJAIL.<pid>) before the child execs, and the
// cgroup id (bpf_get_current_cgroup_id) needs no PID-namespace translation, so
// the in-kernel match is a single hash lookup. See internal/tracer/doc.go.
//
// Deliberately uses only stable tracepoint-context fields and bpf helpers — no
// CO-RE reads of kernel structs — so it does not need a generated vmlinux.h.
// The minimal trace_event_raw_sys_enter below mirrors the kernel's stable
// tracepoint ABI (common header, syscall nr, then the 6 syscall args).

// <linux/bpf.h> provides the BPF_MAP_TYPE_* enums and the fixed-width / __be /
// __wsum types <bpf/bpf_helpers.h> needs; <bpf/bpf_helpers.h> provides SEC(),
// the BTF map macros and the helper prototypes. This avoids a generated
// vmlinux.h: the program reads only the stable tracepoint context (defined
// below) and calls helpers, so it needs no CO-RE kernel-struct relocations. The
// bpf2go invocation adds the multiarch include dir so <asm/types.h> resolves
// under clang's bpf target (see the //go:generate line in tracer_linux.go).
#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>

char LICENSE[] SEC("license") = "GPL";

#define FILENAME_MAX_LEN 256

// Stable layout of the syscall-entry tracepoint context. args[1] is the second
// syscall argument: for both openat(dfd, filename, ...) and openat2(dfd,
// filename, ...) that is the user-space filename pointer.
struct trace_entry {
	short unsigned int type;
	unsigned char	   flags;
	unsigned char	   preempt_count;
	int		   pid;
};
struct trace_event_raw_sys_enter {
	struct trace_entry ent;
	long int	   id;
	long unsigned int  args[6];
	char		   __data[0];
};

struct event {
	__u64 cgid;
	__u64 ts_ns;
	__u8  syscall; // 0 = openat, 1 = openat2
	char  filename[FILENAME_MAX_LEN];
};

// struct event is only ever touched through a ringbuf pointer, so clang may omit
// it from the object's BTF — which makes bpf2go's `-type event` fail with
// "collect C types: not found". A dummy global of the type forces BTF emission.
const struct event *unused_event __attribute__((unused));

// Ring buffer for events -> user space (1 MiB; python emits a burst of opens at
// interpreter startup, so size generously).
struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 1 << 20);
} events SEC(".maps");

// Cgroup ids user space wants traced. Key = cgroup id, value = unused marker.
// User space Put()s a run's cgroup id on start and Delete()s it on completion.
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 1024);
	__type(key, __u64);
	__type(value, __u8);
} active_cgroups SEC(".maps");

static __always_inline int handle(const char *user_fname, __u8 which)
{
	__u64 cgid = bpf_get_current_cgroup_id();
	if (!bpf_map_lookup_elem(&active_cgroups, &cgid))
		return 0; // not a task belonging to a traced run

	struct event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e)
		return 0;

	e->cgid = cgid;
	e->ts_ns = bpf_ktime_get_ns();
	e->syscall = which;
	// The filename pointer lives in user memory at syscall entry.
	bpf_probe_read_user_str(&e->filename, sizeof(e->filename), user_fname);

	bpf_ringbuf_submit(e, 0);
	return 0;
}

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
