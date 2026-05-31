## Runner interface vs calling exec directly

**Context:**
needed to run subprocesses for python and c++. stage 3
needs nsjail sandboxing which is a completely different
way to run processes.

**Options considered:**
1. call exec.Cmd directly in the handler
2. put execution behind a Runner interface

**Decision:**
Runner interface with SubprocessRunner as stage 1 impl

**Rationale:**
if exec.Cmd is called directly in the handler, adding
nsjail in stage 3 means touching the handler and all
tests. with an interface, stage 3 is just a new struct.
also makes handler testable with a fake runner.


## Language config as structs vs if/else in handler

**Context:**
python just runs, c++ needs a compile step first.
needed a way to handle this difference cleanly.

**Options considered:**
1. if/else in the handler checking language name
2. Language struct with a nil Build pointer for
   interpreted languages

**Decision:**
Language struct with Build as a pointer

**Rationale:**
if/else means the handler knows language-specific
details — filenames, commands, timeouts. gets messy
fast. nil Build pointer keeps the handler clean,
it just checks if Build is nil and skips compilation.
also easier to add languages later without touching
the handler.


## Filename validation in handler vs in runner

**Context:**
source_filename comes in from the request. a bad
filename like ../../etc/passwd could escape the temp
directory if not caught early.

**Options considered:**
1. validate in the runner before writing the file
2. validate in the handler before doing anything

**Decision:**
validate in the handler

**Rationale:**
validation failure should return a 400 error before
any filesystem work happens. runner shouldn't be
responsible for request validation — it just runs
what it's given. catching it in the handler keeps
the boundary clean.


## net/http vs chi or gin

**Context:**
needed an HTTP router for two endpoints.

**Options considered:**
1. net/http standard library
2. chi or gin third party router

**Decision:**
net/http

**Rationale:**
two endpoints dont justify a dependency. Go 1.22
ServeMux handles method matching natively. if more
routes get added in later stages switching to chi
is one file change.nex