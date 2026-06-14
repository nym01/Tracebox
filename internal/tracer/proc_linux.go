//go:build linux

package tracer

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// childDiscoveryWindow bounds how long Attach waits for the sandboxed child (and
// its cgroup) to appear after nsjail is spawned. nsjail clones the child and
// places it in its cgroup within microseconds, so the child is almost always
// found on the first probe; the window only matters if nsjail fails to spawn one
// (then Attach gives up and the step is simply not traced). It is deliberately
// short so a failed discovery cannot add meaningful per-request latency.
const childDiscoveryWindow = 150 * time.Millisecond

// childCgroupID finds the sandboxed child that nsjail (running as nsjailPID)
// cloned and returns the cgroup-v2 id it was placed in. nsjail puts the child in
// a fresh cgroup (<mount>/NSJAIL.<childpid>) and adds it to cgroup.procs *before*
// letting it exec, and every language sets a memory/pids/cpu limit, so a
// dedicated cgroup always exists. The id returned here equals
// bpf_get_current_cgroup_id() for that child (both are the cgroup directory's
// inode), so it is the key the eBPF active_cgroups filter matches on.
func childCgroupID(nsjailPID int) (uint64, bool) {
	deadline := time.Now().Add(childDiscoveryWindow)
	for {
		if child, ok := firstChild(nsjailPID); ok {
			if cgid, ok := cgroupID(child); ok {
				return cgid, true
			}
		}
		if time.Now().After(deadline) {
			return 0, false
		}
		time.Sleep(time.Millisecond)
	}
}

// firstChild returns the first direct child PID of pid, read from
// /proc/<pid>/task/<pid>/children. The goboxd container sees nsjail's children
// in its own PID namespace, and nsjail spawns exactly one sandboxed child per
// run, so the first entry is that child.
func firstChild(pid int) (int, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/task/" + strconv.Itoa(pid) + "/children")
	if err != nil {
		return 0, false
	}
	for _, f := range strings.Fields(string(data)) {
		if c, err := strconv.Atoi(f); err == nil {
			return c, true
		}
	}
	return 0, false
}

// cgroupID returns the cgroup-v2 id of pid: the inode of its cgroup directory
// under /sys/fs/cgroup, which is exactly what bpf_get_current_cgroup_id()
// reports. The unified-hierarchy path comes from the "0::<path>" line of
// /proc/<pid>/cgroup (host-absolute under --cgroupns=host, which goboxd runs).
func cgroupID(pid int) (uint64, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return 0, false
	}
	var rel string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "0::") {
			rel = strings.TrimPrefix(line, "0::")
			break
		}
	}
	if rel == "" {
		return 0, false
	}
	var st syscall.Stat_t
	if err := syscall.Stat("/sys/fs/cgroup"+rel, &st); err != nil {
		return 0, false
	}
	return st.Ino, true
}
