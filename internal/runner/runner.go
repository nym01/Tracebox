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
