## Runner interface instead of calling exec directly

**Context:**
was writing the handler and needed to run subprocesses.
realised stage 3 needs nsjail which works completely
differently from just calling exec.

**Options considered:**
1. call exec.Cmd directly in the handler
2. put execution behind a Runner interface

**Decision:**
Runner interface with SubprocessRunner for stage 1

**Rationale:**
if exec is called directly, adding nsjail in stage 3
means touching the handler and rewriting tests. with
an interface its just a new struct. also makes the
handler testable without spawning real processes.


## Language struct instead of if/else per language

**Context:**
python just runs, c++ needs a compile step. needed
to handle this difference without the handler knowing
too much about each language.

**Options considered:**
1. if/else in the handler checking language name
2. Language struct with nil Build pointer

**Decision:**
Language struct with Build as a pointer

**Rationale:**
if/else means the handler knows filenames, commands,
timeouts for each language. gets messy with two, worse
with three. nil Build pointer keeps the handler clean —
check if Build is nil, skip compilation, done.


## filename validation in handler not in runner

**Context:**
source_filename comes from the request. a bad value
like ../../etc/passwd could escape the temp directory.

**Options considered:**
1. validate in the runner before writing the file
2. validate in the handler and reject early with 400

**Decision:**
validate in the handler

**Rationale:**
validation failure should return a 400 before any
filesystem work happens. runner shouldnt care about
request validation, it just runs what its given.


## net/http instead of chi or gin

**Context:**
needed an HTTP router. only have two endpoints for
stage 1.

**Options considered:**
1. net/http standard library
2. chi or gin

**Decision:**
net/http

**Rationale:**
two endpoints dont justify adding a dependency.
Go 1.22 ServeMux handles method matching natively.
if more routes get added in later stages switching
to chi is a small change.