package status

// Build status values
const (
	BuildOK            = "ok"
	BuildFailed        = "failed"
	BuildInternalError = "internal_error"
)

// Test status values
const (
	Accepted                 = "accepted"
	WrongOutput              = "wrong_output"
	OutputWhitespaceMismatch = "output_whitespace_mismatch"
	TimeExceeded             = "time_exceeded"
	MemoryExceeded           = "memory_exceeded"
	RuntimeError             = "runtime_error"
	NotExecuted              = "not_executed"
	InternalError            = "internal_error"
)

// TopLevel computes the top-level run status from build and per-test results.
// If build failed or errored, tests are irrelevant. Otherwise the first
// non-accepted test status wins; all accepted → "accepted".
func TopLevel(buildStatus string, testStatuses []string) string {
	switch buildStatus {
	case BuildFailed:
		return "build_failed"
	case BuildInternalError:
		return InternalError
	}
	for _, s := range testStatuses {
		if s != Accepted {
			return s
		}
	}
	return Accepted
}
