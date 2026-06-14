//go:build !linux

package tracer

import "errors"

// On non-Linux platforms the eBPF tracer is unavailable. These stubs carry the
// same exported surface as tracer_linux.go so the rest of goboxd builds and runs
// (with file-open tracing disabled) on a developer's machine. The production
// build target is the privileged Linux container.

// Tracer is a no-op on non-Linux platforms.
type Tracer struct{}

// Run is a no-op on non-Linux platforms.
type Run struct{}

// Start reports that tracing is unsupported here; the caller logs and continues.
func Start() (*Tracer, error) {
	return nil, errors.New("tracer: eBPF file-open tracing is only supported on linux")
}

// NewRun returns a nil (no-op) Run. Safe on a nil Tracer.
func (t *Tracer) NewRun() *Run { return nil }

// Stop is a no-op. Safe on a nil Tracer.
func (t *Tracer) Stop() error { return nil }

// Attach is a no-op. Safe on a nil Run.
func (r *Run) Attach(nsjailPID int) {}

// Events returns nil. Safe on a nil Run.
func (r *Run) Events() []Event { return nil }

// Close is a no-op. Safe on a nil Run.
func (r *Run) Close() {}
