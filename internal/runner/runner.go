package runner

import "context"

// RunSpec describes a single subprocess invocation.
type RunSpec struct {
	Cmd         string
	Args        []string
	Stdin       string
	WorkDir     string
	WallTimeSec int
	// MemoryKB is the resident-memory limit for the process, in kilobytes.
	// Zero means "no explicit limit". SubprocessRunner ignores this field
	// (it only reports peak memory); NsjailRunner enforces it as a cgroup v2
	// memory.max limit (converted to bytes). It is deliberately NOT enforced
	// via --rlimit_as: that caps *virtual* address space, which managed
	// runtimes (the JVM, V8) reserve far in excess of their real footprint, so
	// an rlimit_as tight enough to matter aborts them before any user code runs.
	MemoryKB int
	// MaxProcesses is the maximum number of processes (tasks) the run may have
	// alive at once. Zero means "no explicit limit". SubprocessRunner ignores
	// this field; NsjailRunner enforces it as a cgroup v2 pids.max limit via
	// --cgroup_pids_max. Unlike the memory limit, exceeding it does not kill the
	// process: the kernel makes fork()/clone() fail with EAGAIN once pids.max is
	// reached, so the sandboxed program observes a failed syscall (and typically
	// exits non-zero → runtime_error) rather than being SIGKILLed the way an OOM
	// is. The count includes every task in the sandbox's cgroup (the run's own
	// process plus any children/threads it spawns).
	MaxProcesses int
	// CPUMsPerSec is the CPU bandwidth limit for the run, in milliseconds of CPU
	// time the sandbox may use per wall-clock second (1000 == one core, 2000 == two
	// cores). Zero means "no explicit limit". SubprocessRunner ignores this field;
	// NsjailRunner enforces it as a cgroup v2 cpu.max limit via
	// --cgroup_cpu_ms_per_sec. Like MaxProcesses, exceeding it does not kill the
	// process: the kernel simply *throttles* the cgroup (its tasks are scheduled for
	// at most CPUMsPerSec ms of CPU per second), so a CPU-spinner does less useless
	// work but is otherwise unaffected — it is the wall-time limit, not this one,
	// that ultimately ends a spinner. The point is bounding the per-request CPU draw
	// so concurrent requests cannot saturate every host core (CPU-exhaustion DoS).
	CPUMsPerSec int
}

// RunResult holds what came back from the subprocess.
type RunResult struct {
	Stdout       string
	Stderr       string
	DurationMs   int64
	MemoryPeakKB int64
	ExitCode     int
	TimedOut     bool
	// MemoryExceeded reports that the process was killed by the cgroup memory
	// limit (out of memory), as opposed to a wall-clock timeout (TimedOut) or a
	// plain non-zero exit. Only NsjailRunner sets it; SubprocessRunner leaves it
	// false. The handler maps it to the memory_exceeded status.
	MemoryExceeded bool
}

// Runner executes a command and returns its result.
// Stage 3 will swap SubprocessRunner for an NsjailRunner without changing callers.
type Runner interface {
	Run(ctx context.Context, spec RunSpec) (RunResult, error)
}
