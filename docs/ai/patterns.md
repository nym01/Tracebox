## defer cleanup right after creation

**Context:**
was getting temp directories leaking during testing.
took a while to figure out that putting os.RemoveAll
at the end of the function doesnt work if the function
returns early on an error.

**Pattern:**
the moment you create a temp dir, the very next line
should be defer os.RemoveAll. not after the if block,
not at the end. right after creation. defer runs on
every return path so you dont have to think about it.

**Where we used it:**
internal/api/handlers.go — learned this after noticing
temp dirs were not getting cleaned up when validation
failed early.


## context.WithTimeout handles process killing for you

**Context:**
needed to kill user code after a time limit. thought
I would need a goroutine that waits and then kills
the process manually.

**Pattern:**
turns out exec.CommandContext does this automatically.
pass it a context with a deadline and it kills the
process when time runs out. no goroutine needed at all.
only learned this after asking how timeout detection
works in go.

**Where we used it:**
internal/runner/subprocess.go — wall time limit from
RunSpec wrapped into context.WithTimeout.


## nil pointer for optional config

**Context:**
python has no build step, c++ does. first thought was
a boolean HasBuild flag but then the flag and the
config could get out of sync.

**Pattern:**
make the config a pointer instead. nil means it doesnt
exist. handler just checks if Build is nil. bonus: 
omitempty on a pointer actually works — nil pointer
disappears from JSON. learned the hard way that
omitempty on a plain struct doesnt omit it, shows
up as {} instead.

**Where we used it:**
internal/language/language.go and handlers.go —
also fixed the build field showing up in python
responses after learning this.


## Runner interface so tests dont need real processes

**Context:**
wanted to test status mapping and output comparison
without actually running python or c++. wasnt sure
how to do this cleanly.

**Pattern:**
put execution behind an interface. tests inject a
fake that returns whatever result you want. no real
subprocess needed to test the logic around it.
also means stage 3 can swap in nsjail without
touching the handler.

**Where we used it:**
internal/runner/runner.go interface definition,
internal/api/handlers_test.go fakeRunner used in
all handler tests.


## cap output or memory fills up

**Context:**
didnt think about this upfront. realised a program
that prints forever would fill memory before the
timeout fires.

**Pattern:**
custom io.Writer that counts bytes written and stops
accepting after a limit. append a truncation marker
so the caller knows output was cut. plug it in as
cmd.Stdout and cmd.Stderr.

**Where we used it:**
internal/runner/subprocess.go — 64KB limit on both
stdout and stderr. found out about this from the
spec security section.