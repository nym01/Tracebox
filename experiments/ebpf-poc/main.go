// Command ebpf-poc is an EXPLORATORY proof-of-concept for Phase 4 (eBPF file-open
// monitoring). It is NOT wired into the goboxd API, runner, CLI, or MCP server —
// it lives in experiments/ on its own Go module so it touches none of the
// production dependency graph.
//
// What it does: attaches eBPF programs to the openat(2)/openat2(2) syscall-entry
// tracepoints, filters in-kernel by a single PID passed on the command line, and
// prints (timestamp, pid, syscall, filename) for every matching open as it
// happens. A PID of 0 means "trace every process" (useful for a smoke test).
//
// Usage:
//
//	sudo ./ebpf-poc <pid>      # trace one process
//	sudo ./ebpf-poc 0          # trace everything (firehose)
//
// Requires CAP_BPF + CAP_PERFMON (or root) and a kernel with BTF
// (/sys/kernel/btf/vmlinux). See README.md for the build steps.
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

// generate the Go bindings + embedded BPF object from trace.bpf.c.
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -type event -type filter_cfg trace trace.bpf.c

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <pid>   (0 = trace all)\n", os.Args[0])
		os.Exit(2)
	}
	pid, err := strconv.ParseUint(os.Args[1], 10, 32)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid pid %q: %v\n", os.Args[1], err)
		os.Exit(2)
	}

	// eBPF maps/programs are accounted against the locked-memory rlimit on older
	// kernels; lift it so loading never fails for that reason.
	if err := rlimit.RemoveMemlock(); err != nil {
		fatal("remove memlock rlimit: %v", err)
	}

	objs := traceObjects{}
	if err := loadTraceObjects(&objs, nil); err != nil {
		fatal("load BPF objects: %v", err)
	}
	defer objs.Close()

	// Build the kernel-side filter. For a real pid we cannot just hand the kernel
	// the number we were given: under Docker Desktop / WSL2 this process and the
	// kernel are in different pid namespaces, so our pid != the global pid eBPF
	// sees (and nsjail nests the sandboxed child one namespace deeper still). So
	// we identify the target's pid *namespace* by the (dev, inode) of
	// /proc/<pid>/ns/pid and let bpf_get_ns_current_pid_tgid() compare the pid
	// within that namespace. pid 0 stays a plain "trace everything" firehose.
	cfg := traceFilterCfg{Nspid: uint32(pid)}
	if pid != 0 {
		var st syscall.Stat_t
		nsPath := fmt.Sprintf("/proc/%d/ns/pid", pid)
		if err := syscall.Stat(nsPath, &st); err != nil {
			fatal("stat %s (is the pid alive and visible?): %v", nsPath, err)
		}
		cfg.Dev = st.Dev
		cfg.Ino = st.Ino
	}
	if err := objs.Filter.Put(uint32(0), &cfg); err != nil {
		fatal("set filter config: %v", err)
	}

	lOpenat, err := link.Tracepoint("syscalls", "sys_enter_openat", objs.TpOpenat, nil)
	if err != nil {
		fatal("attach sys_enter_openat: %v", err)
	}
	defer lOpenat.Close()

	lOpenat2, err := link.Tracepoint("syscalls", "sys_enter_openat2", objs.TpOpenat2, nil)
	if err != nil {
		fatal("attach sys_enter_openat2: %v", err)
	}
	defer lOpenat2.Close()

	rd, err := ringbuf.NewReader(objs.Events)
	if err != nil {
		fatal("open ringbuf reader: %v", err)
	}
	defer rd.Close()

	// Unblock the blocking ringbuf Read() on Ctrl-C / SIGTERM.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		rd.Close()
	}()

	if pid == 0 {
		fmt.Fprintln(os.Stderr, "tracing openat/openat2 for ALL pids (Ctrl-C to stop)")
	} else {
		fmt.Fprintf(os.Stderr, "tracing openat/openat2 for pid %d (Ctrl-C to stop)\n", pid)
	}

	syscallName := map[uint8]string{0: "openat", 1: "openat2"}
	var ev traceEvent
	for {
		rec, err := rd.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			fmt.Fprintf(os.Stderr, "ringbuf read: %v\n", err)
			continue
		}
		if err := binary.Read(bytes.NewReader(rec.RawSample), binary.LittleEndian, &ev); err != nil {
			fmt.Fprintf(os.Stderr, "decode event: %v\n", err)
			continue
		}
		fmt.Printf("%s pid=%-7d %-7s %s\n",
			time.Now().Format("15:04:05.000"),
			ev.Pid,
			syscallName[ev.Syscall],
			unix2str(ev.Filename[:]),
		)
	}
}

// unix2str converts a NUL-terminated C char array into a Go string.
func unix2str(b []int8) string {
	u := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		u = append(u, byte(c))
	}
	return string(u)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ebpf-poc: "+format+"\n", args...)
	os.Exit(1)
}
