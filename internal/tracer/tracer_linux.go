//go:build linux

package tracer

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"
)

// Regenerate the Go bindings + embedded BPF object from trace.bpf.c. Requires
// clang + libbpf headers (libbpf-dev) + linux uapi headers (linux-libc-dev); run
// inside the build container, not on the host. See the Dockerfile builder stage.
// The trailing -I points clang's bpf target at Debian's multiarch include dir so
// <asm/types.h> (pulled in by <linux/bpf.h>) resolves; the build image is amd64.
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -type event trace trace.bpf.c -- -I/usr/include/x86_64-linux-gnu

// tracefsPath is where link.Tracepoint() resolves tracepoints; the goboxd
// container does not mount it by default, so Start mounts it (privileged).
const tracefsPath = "/sys/kernel/tracing"

// syscallName maps the eBPF syscall tag to a name.
var syscallName = map[uint8]string{0: "openat", 1: "openat2"}

// Tracer is the process-wide, persistent file-open monitor. It owns the eBPF
// programs/maps, the ring-buffer reader goroutine, and the registry mapping a
// live run's cgroup id to the Run collecting its events.
type Tracer struct {
	objs   traceObjects
	links  []link.Link
	reader *ringbuf.Reader

	mu   sync.RWMutex
	runs map[uint64]*Run // cgroup id -> run collecting that cgroup's opens
}

// Start mounts tracefs (if needed), loads the eBPF programs, attaches them to
// the openat/openat2 tracepoints, and starts draining the ring buffer. The
// returned Tracer stays attached until Stop. Requires CAP_BPF/CAP_PERFMON and a
// kernel with BTF (true for the privileged goboxd container); on failure the
// caller should log and continue with tracing disabled.
func Start() (*Tracer, error) {
	if err := mountTracefs(); err != nil {
		return nil, fmt.Errorf("tracer: mount tracefs: %w", err)
	}
	// eBPF maps/programs are accounted against the locked-memory rlimit on older
	// kernels; lift it so loading never fails for that reason.
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("tracer: remove memlock: %w", err)
	}

	t := &Tracer{runs: make(map[uint64]*Run)}
	if err := loadTraceObjects(&t.objs, nil); err != nil {
		return nil, fmt.Errorf("tracer: load bpf objects: %w", err)
	}

	lOpenat, err := link.Tracepoint("syscalls", "sys_enter_openat", t.objs.TpOpenat, nil)
	if err != nil {
		t.objs.Close()
		return nil, fmt.Errorf("tracer: attach sys_enter_openat: %w", err)
	}
	t.links = append(t.links, lOpenat)

	lOpenat2, err := link.Tracepoint("syscalls", "sys_enter_openat2", t.objs.TpOpenat2, nil)
	if err != nil {
		t.close()
		return nil, fmt.Errorf("tracer: attach sys_enter_openat2: %w", err)
	}
	t.links = append(t.links, lOpenat2)

	rd, err := ringbuf.NewReader(t.objs.Events)
	if err != nil {
		t.close()
		return nil, fmt.Errorf("tracer: open ringbuf reader: %w", err)
	}
	t.reader = rd

	go t.readLoop()
	return t, nil
}

// readLoop drains the ring buffer and routes each event to the run that owns its
// cgroup id, until the reader is closed by Stop.
func (t *Tracer) readLoop() {
	var ev traceEvent
	for {
		rec, err := t.reader.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			continue
		}
		if err := binary.Read(bytes.NewReader(rec.RawSample), binary.LittleEndian, &ev); err != nil {
			continue
		}
		t.mu.RLock()
		run := t.runs[ev.Cgid]
		t.mu.RUnlock()
		if run == nil {
			continue // event for a cgroup no longer (or not yet) registered
		}
		run.add(Event{
			Syscall: syscallName[ev.Syscall],
			Path:    cstr(ev.Filename[:]),
			Time:    time.Now(),
		})
	}
}

// register starts routing events for cgid to run.
func (t *Tracer) register(cgid uint64, run *Run) {
	t.mu.Lock()
	t.runs[cgid] = run
	t.mu.Unlock()
	// Best-effort: a full map (>1024 concurrent cgroups) just means this run's
	// events are not captured, never an error that should fail the run.
	_ = t.objs.ActiveCgroups.Put(cgid, uint8(1))
}

// unregister stops routing events for cgid and frees the kernel-side entry.
func (t *Tracer) unregister(cgid uint64) {
	t.mu.Lock()
	delete(t.runs, cgid)
	t.mu.Unlock()
	_ = t.objs.ActiveCgroups.Delete(cgid)
}

// Stop detaches everything and stops the reader goroutine. Safe on a nil Tracer.
func (t *Tracer) Stop() error {
	if t == nil {
		return nil
	}
	if t.reader != nil {
		t.reader.Close() // unblocks readLoop with ErrClosed
	}
	return t.close()
}

// close releases the links and objects (used by Stop and by Start's error path).
func (t *Tracer) close() error {
	for _, l := range t.links {
		l.Close()
	}
	t.links = nil
	return t.objs.Close()
}

// Run collects the file opens of one /run request, across its (possibly several)
// nsjail invocations — one per build/test step, each in its own cgroup.
type Run struct {
	tracer *Tracer
	mu     sync.Mutex
	cgids  []uint64
	events []Event
}

// NewRun begins collecting trace events for a /run request. Safe on a nil
// Tracer (tracing disabled): it returns a nil *Run whose methods are no-ops.
func (t *Tracer) NewRun() *Run {
	if t == nil {
		return nil
	}
	return &Run{tracer: t}
}

// Attach discovers the sandboxed child that nsjail (PID nsjailPID) spawned and
// registers its cgroup so this run collects the child's file opens. It is meant
// to be wired as runner.RunSpec.OnStart, which the nsjail runner calls just
// after spawn. Safe on a nil *Run.
func (r *Run) Attach(nsjailPID int) {
	if r == nil {
		return
	}
	cgid, ok := childCgroupID(nsjailPID)
	if !ok {
		return // child or its cgroup not found in time; events for this step missed
	}
	r.mu.Lock()
	r.cgids = append(r.cgids, cgid)
	r.mu.Unlock()
	r.tracer.register(cgid, r)
}

// add appends an observed event (called from the reader goroutine).
func (r *Run) add(e Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

// Events returns a snapshot of the opens collected so far. Safe on a nil *Run
// (returns nil).
func (r *Run) Events() []Event {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}

// Close stops collecting and frees the kernel-side filter entries for this run.
// Safe on a nil *Run. Idempotent.
func (r *Run) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cgids := r.cgids
	r.cgids = nil
	r.mu.Unlock()
	for _, cgid := range cgids {
		r.tracer.unregister(cgid)
	}
}

// mountTracefs mounts tracefs at tracefsPath if it is not already available.
// link.Tracepoint() needs it; the container does not mount it by default.
func mountTracefs() error {
	if _, err := os.Stat(tracefsPath + "/events"); err == nil {
		return nil // already mounted
	}
	if err := unix.Mount("none", tracefsPath, "tracefs", 0, ""); err != nil {
		// Fall back to debugfs, which also exposes tracing/events.
		if _, derr := os.Stat("/sys/kernel/debug/tracing/events"); derr == nil {
			return nil
		}
		return err
	}
	return nil
}

// cstr converts a NUL-terminated C char array into a Go string.
func cstr(b []int8) string {
	u := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		u = append(u, byte(c))
	}
	return string(u)
}
