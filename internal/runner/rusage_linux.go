//go:build linux

package runner

import (
	"os/exec"
	"syscall"
)

// peakMemoryKB returns ru_maxrss from the child's process state.
// On Linux ru_maxrss is already in kilobytes.
func peakMemoryKB(cmd *exec.Cmd) int64 {
	if cmd.ProcessState == nil {
		return 0
	}
	if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		return ru.Maxrss
	}
	return 0
}
