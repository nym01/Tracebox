package runner

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"
)

const outputCap = 64 * 1024

// SubprocessRunner is the Stage 1 implementation: plain exec.Cmd + context timeout.
type SubprocessRunner struct{}

// Backend names the sandbox implementation for reporting (see handlers' backendName).
func (SubprocessRunner) Backend() string { return "subprocess" }

func (SubprocessRunner) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	wallSec := spec.WallTimeSec
	if wallSec <= 0 {
		wallSec = 10
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(wallSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, spec.Cmd, spec.Args...)
	cmd.Dir = spec.WorkDir
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

	timedOut := runCtx.Err() == context.DeadlineExceeded

	var exitCode int
	var memKB int64
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
		memKB = peakMemoryKB(cmd)
	}

	res := RunResult{
		Stdout:       string(outW.Bytes()),
		Stderr:       string(errW.Bytes()),
		DurationMs:   durationMs,
		MemoryPeakKB: memKB,
		ExitCode:     exitCode,
		TimedOut:     timedOut,
	}

	if timedOut {
		return res, nil
	}
	if waitErr != nil {
		if _, ok := waitErr.(*exec.ExitError); !ok {
			return res, waitErr
		}
	}
	return res, nil
}

// cappedWriter accepts up to limit bytes then silently drops further writes,
// appending a truncation marker at the cap boundary.
type cappedWriter struct {
	buf    bytes.Buffer
	limit  int
	capped bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if w.capped {
		return len(p), nil
	}
	remaining := w.limit - w.buf.Len()
	if len(p) <= remaining {
		return w.buf.Write(p)
	}
	w.buf.Write(p[:remaining])
	w.buf.WriteString("\n[output truncated]")
	w.capped = true
	return len(p), nil
}

func (w *cappedWriter) Bytes() []byte {
	return w.buf.Bytes()
}
