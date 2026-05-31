## defer cleanup right after creation, not at the end

**Context:**
needed temp directories cleaned up on every exit path —
early returns, errors, panics. putting os.RemoveAll at
the end of the function misses early returns.

**Pattern:**
create the temp dir, then immediately defer os.RemoveAll
on the next line before doing anything else with it.

**Where we used it:**
internal/api/handlers.go — tmpDir is created then
defer os.RemoveAll fires on every exit path including
early validation failures and build errors.


## context.WithTimeout kills subprocess automatically

**Context:**
needed to enforce a wall time limit on user code without
manually managing goroutines or kill signals.

**Pattern:**
wrap the parent context with context.WithTimeout and pass
it to exec.CommandContext. when the deadline fires the
process gets killed automatically, no extra code needed.

**Where we used it:**
internal/runner/subprocess.go — wall time limit comes
from RunSpec, gets converted to a timeout duration and
wrapped around the exec call.


## nil pointer as config and control flow in one

**Context:**
interpreted languages have no build step, compiled ones
do. needed a way to represent this without a separate
boolean flag that could get out of sync with the actual
config.

**Pattern:**
make the optional config a pointer. nil means absent.
the handler checks nil to skip the build phase, and
omitempty means the field disappears from JSON responses
automatically when nil.

**Where we used it:**
internal/language/language.go Language.Build field,
internal/api/handlers.go build phase check.


## interface for testability and future swap

**Context:**
handler needed to run subprocesses but tests shouldnt
spawn real processes. also stage 3 needs nsjail which
is a different execution backend.

**Pattern:**
define a Runner interface, depend on that in the handler.
tests inject a fake implementation, production uses
SubprocessRunner. swapping backends later means one
assignment change.

**Where we used it:**
internal/runner/runner.go Runner interface,
internal/api/handlers.go handler takes a Runner,
internal/api/handlers_test.go fakeRunner injects
controlled results.


## capped writer to bound subprocess output

**Context:**
a runaway process could write unlimited stdout and fill
memory. needed a hard cap without killing the process
early or breaking the io.Writer interface.

**Pattern:**
wrap a bytes.Buffer in a custom Writer that accepts up
to N bytes then drops the rest and appends a truncation
marker. set it as cmd.Stdout and cmd.Stderr instead of a plain buffer.

**Where we used it:**
internal/runner/subprocess.go — cappedWriter with a
64KB limit on both stdout and stderr for every run.
