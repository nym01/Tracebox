//go:build ignore

// trace.bpf.c — Phase 4 file-open + process-spawn tracer (production, wired into
// goboxd).
//
// Hooks the openat(2)/openat2(2) and execve(2)/execveat(2) syscall-entry
// tracepoints and emits records over two ring buffers — file opens on `events`,
// process spawns on `exec_events` — for every syscall made by a task whose
// cgroup id is registered in the active_cgroups map. User space (internal/tracer)
// registers a run's cgroup id when the run starts and removes it when the run
// ends, so only the sandboxed children of in-flight /run requests produce events
// — the rest of the host is filtered out in-kernel.
//
// The two event families use separate ring buffers (rather than one tagged
// buffer) so the high-frequency open stream and the much rarer exec stream stay
// independent: an interpreter-startup burst of opens never pushes the larger
// exec records (which carry argv) out of a shared buffer, and the file-open path
// stays byte-for-byte unchanged from the Phase-4a tracer.
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
// argv capture bounds. The kernel passes argv as a NULL-terminated array of user
// pointers; we copy at most ARGV_MAX of them, each truncated to ARG_LEN bytes.
// These caps keep the event a fixed size and keep the in-kernel copy loop within
// the BPF verifier's instruction/complexity budget (the loop is fully unrolled).
// Capturing the executable name + the first handful of arguments is enough to
// recognise the spawn (e.g. system("/bin/ls -la")); fuller argv would balloon
// the record and risk verifier rejection on this kernel — see internal/tracer.
#define ARGV_MAX 8
#define ARG_LEN  64

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

// One process-spawn (execve/execveat). filename is the program being executed;
// args holds up to ARGV_MAX captured arguments (argv[0..argc-1]) and argc is how
// many were actually copied before the NULL terminator (or the ARGV_MAX cap).
struct exec_event {
	__u64 cgid;
	__u64 ts_ns;
	__u32 argc;
	__u8  syscall; // 0 = execve, 1 = execveat
	char  filename[FILENAME_MAX_LEN];
	char  args[ARGV_MAX][ARG_LEN];
};

// struct event / struct exec_event are only ever touched through a ringbuf
// pointer, so clang may omit them from the object's BTF — which makes bpf2go's
// `-type` fail with "collect C types: not found". Dummy globals of each type
// force BTF emission.
const struct event *unused_event __attribute__((unused));
const struct exec_event *unused_exec_event __attribute__((unused));

// Ring buffer for file-open events -> user space (1 MiB; python emits a burst of
// opens at interpreter startup, so size generously).
struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 1 << 20);
} events SEC(".maps");

// Ring buffer for process-spawn events -> user space. Execs are far rarer than
// opens (a few per run), so a smaller buffer is plenty even though each record
// is larger (it carries argv).
struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 1 << 18);
} exec_events SEC(".maps");

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

static __always_inline int handle_exec(const char *user_fname,
				       const char *const *user_argv, __u8 which)
{
	__u64 cgid = bpf_get_current_cgroup_id();
	if (!bpf_map_lookup_elem(&active_cgroups, &cgid))
		return 0; // not a task belonging to a traced run

	struct exec_event *e = bpf_ringbuf_reserve(&exec_events, sizeof(*e), 0);
	if (!e)
		return 0;

	e->cgid = cgid;
	e->ts_ns = bpf_ktime_get_ns();
	e->syscall = which;
	e->argc = 0;
	// The filename pointer lives in user memory at syscall entry.
	bpf_probe_read_user_str(&e->filename, sizeof(e->filename), user_fname);

	// Copy up to ARGV_MAX argv entries. argv is a user array of user string
	// pointers terminated by NULL; read each slot, stop at the terminator, and
	// copy the pointed-to string. The loop is fully unrolled so every index is a
	// compile-time constant and the verifier sees only constant-offset writes
	// into the reserved record.
#pragma unroll
	for (int i = 0; i < ARGV_MAX; i++) {
		const char *argp = NULL;
		if (bpf_probe_read_user(&argp, sizeof(argp), &user_argv[i]) != 0)
			break;
		if (!argp)
			break; // NULL terminator: no more arguments
		bpf_probe_read_user_str(e->args[i], ARG_LEN, argp);
		e->argc = i + 1;
	}

	bpf_ringbuf_submit(e, 0);
	return 0;
}

// execve(const char *filename, const char *const *argv, ...) — filename is
// args[0], argv is args[1].
SEC("tracepoint/syscalls/sys_enter_execve")
int tp_execve(struct trace_event_raw_sys_enter *ctx)
{
	return handle_exec((const char *)ctx->args[0],
			   (const char *const *)ctx->args[1], 0);
}

// execveat(int dfd, const char *filename, const char *const *argv, ...) —
// filename is args[1], argv is args[2].
SEC("tracepoint/syscalls/sys_enter_execveat")
int tp_execveat(struct trace_event_raw_sys_enter *ctx)
{
	return handle_exec((const char *)ctx->args[1],
			   (const char *const *)ctx->args[2], 1);
}
