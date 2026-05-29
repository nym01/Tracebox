//go:build !linux

package runner

import "os/exec"

func peakMemoryKB(_ *exec.Cmd) int64 { return 0 }
