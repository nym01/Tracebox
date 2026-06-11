package runner

import "context"

// RunSpec describes a single subprocess invocation.
type RunSpec struct {
	Cmd         string
	Args        []string
	Stdin       string
	WorkDir     string
	WallTimeSec int
	// MemoryKB is the address-space limit for the process, in kilobytes.
	// Zero means "no explicit limit". SubprocessRunner ignores this field
	// (it only reports peak memory); NsjailRunner enforces it via --rlimit_as.
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
}

// Runner executes a command and returns its result.
// Stage 3 will swap SubprocessRunner for an NsjailRunner without changing callers.
type Runner interface {
	Run(ctx context.Context, spec RunSpec) (RunResult, error)
}
