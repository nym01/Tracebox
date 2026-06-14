//go:build ignore

// trace.bpf.c — Phase 4 file-open + process-spawn + network-connect tracer
// (production, wired into goboxd).
//
// Hooks the openat(2)/openat2(2), execve(2)/execveat(2) and connect(2)
// syscall-entry tracepoints and emits records over three ring buffers — file
// opens on `events`, process spawns on `exec_events`, connection attempts on
// `net_events` — for every syscall made by a task whose cgroup id is registered
// in the active_cgroups map. User space (internal/tracer) registers a run's
// cgroup id when the run starts and removes it when the run ends, so only the
// sandboxed children of in-flight /run requests produce events — the rest of the
// host is filtered out in-kernel.
//
// The three event families use separate ring buffers (rather than one tagged
// buffer) so the high-frequency open stream, the much rarer exec stream, and the
// rare-but-tiny connect stream stay independent: an interpreter-startup burst of
// opens never pushes the larger exec records (which carry argv) out of a shared
// buffer; the file-open path stays byte-for-byte unchanged from the Phase-4a
// tracer; and the userspace reader for each buffer decodes a single fixed struct
// type with no per-record discriminator. (Phase 4c chose a third buffer over
// reusing exec_events for this type-safety/independence, even though connect
// events are small and rare — the cost of one more 64 KiB ring is negligible.)
//
// connect(2) is traced for audit visibility, not enforcement: the sandbox's
// network namespace is empty (only lo, no routes), so connect() fails with
// ENETUNREACH — but the destination the sandboxed code *tried* to reach is
// captured at syscall entry, before the kernel rejects it. This records intent
// ("this run tried to reach 8.8.8.8:53") that the failure alone would hide.
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

// Address families we extract a destination from. Other families (AF_UNIX, etc.)
// produce no event — there is no IP/port to report. Values are the stable Linux
// uapi constants.
#define AF_INET  2
#define AF_INET6 10

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

// One connect(2) attempt. family is the socket address family (AF_INET or
// AF_INET6); port is the destination port in network byte order (as it sits in
// the sockaddr — user space converts); addr holds the raw destination address
// bytes — IPv4 in addr[0..3], IPv6 in addr[0..15]. Captured at syscall entry, so
// it records the intended destination even though the connect() ultimately fails
// (the sandbox netns has no route). Only AF_INET/AF_INET6 produce a record.
struct connect_event {
	__u64 cgid;
	__u64 ts_ns;
	__u16 family;
	__u16 port;
	__u8  syscall; // 0 = connect
	__u8  addr[16];
};

// struct event / struct exec_event / struct connect_event are only ever touched
// through a ringbuf pointer, so clang may omit them from the object's BTF — which
// makes bpf2go's `-type` fail with "collect C types: not found". Dummy globals of
// each type force BTF emission.
const struct event *unused_event __attribute__((unused));
const struct exec_event *unused_exec_event __attribute__((unused));
const struct connect_event *unused_connect_event __attribute__((unused));

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

// Ring buffer for network-connect events -> user space. connect() attempts are
// rare (most sandboxed runs make none) and each record is tiny, so a small
// buffer is ample.
struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 1 << 16);
} net_events SEC(".maps");

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

static __always_inline int handle_connect(const void *uaddr, __u8 which)
{
	__u64 cgid = bpf_get_current_cgroup_id();
	if (!bpf_map_lookup_elem(&active_cgroups, &cgid))
		return 0; // not a task belonging to a traced run

	// sa_family is the first field of every sockaddr (sockaddr_in.sin_family /
	// sockaddr_in6.sin6_family). Read it from user memory first so we can skip
	// families that carry no IP/port (AF_UNIX, AF_NETLINK, …) without reserving a
	// record for them.
	__u16 family = 0;
	if (bpf_probe_read_user(&family, sizeof(family), uaddr) != 0)
		return 0;
	if (family != AF_INET && family != AF_INET6)
		return 0;

	struct connect_event *e = bpf_ringbuf_reserve(&net_events, sizeof(*e), 0);
	if (!e)
		return 0;

	e->cgid = cgid;
	e->ts_ns = bpf_ktime_get_ns();
	e->syscall = which;
	e->family = family;
	e->port = 0;
	__builtin_memset(e->addr, 0, sizeof(e->addr));

	// Extract the destination from the sockaddr in user memory. Both layouts put
	// the port at byte offset 2 (network byte order); the address follows at
	// offset 4 (sockaddr_in.sin_addr, 4 bytes) or offset 8 (sockaddr_in6.sin6_addr,
	// after the 4-byte sin6_flowinfo, 16 bytes). Constant sizes keep the verifier
	// happy.
	if (family == AF_INET) {
		bpf_probe_read_user(&e->port, sizeof(e->port), (const __u8 *)uaddr + 2);
		bpf_probe_read_user(e->addr, 4, (const __u8 *)uaddr + 4);
	} else { // AF_INET6
		bpf_probe_read_user(&e->port, sizeof(e->port), (const __u8 *)uaddr + 2);
		bpf_probe_read_user(e->addr, 16, (const __u8 *)uaddr + 8);
	}

	bpf_ringbuf_submit(e, 0);
	return 0;
}

// connect(int sockfd, const struct sockaddr *addr, socklen_t addrlen) — the
// destination sockaddr is args[1]. One tracepoint covers both TCP and UDP
// connect() calls.
SEC("tracepoint/syscalls/sys_enter_connect")
int tp_connect(struct trace_event_raw_sys_enter *ctx)
{
	return handle_connect((const void *)ctx->args[1], 0);
}
