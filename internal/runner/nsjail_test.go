package runner

// Compile-time assertions that both runner implementations satisfy the
// Runner interface. If either type drifts from the interface, the package
// fails to compile.
var (
	_ Runner = NsjailRunner{}
	_ Runner = SubprocessRunner{}
)
