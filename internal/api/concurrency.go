package api

import (
	"os"
	"runtime"
	"strconv"
)

var sem chan struct{}

func init() {
	n := runtime.NumCPU()
	if v := os.Getenv("GOBOXD_MAX_CONCURRENCY"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	sem = make(chan struct{}, n)
}
