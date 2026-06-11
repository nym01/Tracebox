package runner

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// nsjailGraceSec is added to the wall-time limit when sizing the Go context
// deadline. nsjail enforces the real limit internally via --time_limit; the
// outer context only fires as a backstop if nsjail itself hangs.
const nsjailGraceSec = 5

// NsjailRunner runs the command inside an nsjail sandbox. It implements the
// same Runner interface as SubprocessRunner and returns the identical
// RunResult shape, so callers can swap between the two without changes.
//
// This first pass uses a minimal set of nsjail flags: one-shot mode, a wall
// time limit, an address-space limit, and quiet logging. It does NOT yet
// chroot or drop privileges — that is layered on in a later step once basic
// output/exit-code/timeout capture is confirmed to match SubprocessRunner.
type NsjailRunner struct {
	// NsjailPath is the path to the nsjail binary. Defaults to "nsjail"
	// (resolved via PATH) when empty.
	NsjailPath string
}

func (r NsjailRunner) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	wallSec := spec.WallTimeSec
	if wallSec <= 0 {
		wallSec = 10
	}

	// Outer context backstop: a few seconds beyond nsjail's own --time_limit so
	// nsjail kills the child first and we observe that as a timeout. The context
	// only fires if nsjail itself fails to terminate.
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(wallSec+nsjailGraceSec)*time.Second)
	defer cancel()

	nsjailPath := r.NsjailPath
	if nsjailPath == "" {
		nsjailPath = "nsjail"
	}

	nsjailArgs := []string{
		"--mode", "o", // one-shot: run the command once and exit
		"--time_limit", strconv.Itoa(wallSec),
		"--really_quiet", // suppress nsjail's own logging so only child stdio remains
		// By default nsjail creates a fresh mount namespace and mounts an empty
		// tmpfs as "/", which hides the host filesystem — the interpreter/compiler
		// and the per-request WorkDir then disappear and execve fails with ENOENT.
		// Until proper chroot/bind-mount isolation is layered on, disable the mount
		// namespace so the child sees the host filesystem (and writable WorkDir)
		// exactly as SubprocessRunner does. Other namespaces (pid/net/ipc/uts/user/
		// cgroup) remain enabled.
		"--disable_clone_newns",
	}
	// nsjail --rlimit_as takes a value in MB. Convert from KB, rounding up so we
	// never hand the process less than the configured limit.
	if spec.MemoryKB > 0 {
		memMB := (spec.MemoryKB + 1023) / 1024
		nsjailArgs = append(nsjailArgs, "--rlimit_as", strconv.Itoa(memMB))
	}
	// Run inside the per-request working directory so relative artifacts (e.g.
	// ./solution) and the source file resolve exactly as under SubprocessRunner.
	if spec.WorkDir != "" {
		nsjailArgs = append(nsjailArgs, "--cwd", spec.WorkDir)
	}
	// Everything after "--" is the command and its arguments.
	nsjailArgs = append(nsjailArgs, "--", spec.Cmd)
	nsjailArgs = append(nsjailArgs, spec.Args...)

	cmd := exec.CommandContext(runCtx, nsjailPath, nsjailArgs...)
	cmd.Stdin = strings.NewReader(spec.Stdin)

	outW := &cappedWriter{limit: outputCap}
	errW := &cappedWriter{limit: outputCap}
	cmd.Stdout = outW
	cmd.Stderr = errW

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return RunResult{}, err
	}
	waitErr := cmd.Wait()
	durationMs := time.Since(start).Milliseconds()

	var exitCode int
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	// Timeout detection. nsjail enforces --time_limit and SIGKILLs the child at
	// the boundary, so a process that runs up to (or past) the wall limit is a
	// timeout. The outer-context deadline is the backstop for a hung nsjail.
	timedOut := runCtx.Err() == context.DeadlineExceeded
	if !timedOut && durationMs >= int64(wallSec)*1000 {
		timedOut = true
	}

	res := RunResult{
		Stdout:     string(outW.Bytes()),
		Stderr:     string(errW.Bytes()),
		DurationMs: durationMs,
		// MemoryPeakKB is not yet reported under nsjail: ProcessState.SysUsage
		// reflects the nsjail wrapper, not the sandboxed child. Left at 0 until
		// cgroup-based accounting is added in a later step.
		MemoryPeakKB: 0,
		ExitCode:     exitCode,
		TimedOut:     timedOut,
	}

	if timedOut {
		return res, nil
	}
	// A non-zero child exit arrives as *exec.ExitError; that is a normal result
	// (runtime error), not a runner failure. Only surface other errors.
	if waitErr != nil {
		if _, ok := waitErr.(*exec.ExitError); !ok {
			return res, waitErr
		}
	}
	return res, nil
}
