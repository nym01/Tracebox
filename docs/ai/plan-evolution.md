## 2026-05-29 · language registry instead of if/else

**What we thought we'd do:** handle python and c++ with a simple
if/else in the handler, get it working first and clean up later

**What we actually did:** pulled language config into a Language
struct with a Build pointer (nil for interpreted languages) and
a Run config. languages register at startup via Register()

**Why it changed:** once c++ needed a build step the handler was
getting messy — it would need to know filenames, compile commands,
timeouts. the struct approach keeps the handler language-agnostic


## 2026-05-29 · Runner interface

**What we thought we'd do:** call exec.Cmd directly in the handler

**What we actually did:** extracted a Runner interface, subprocess
is one implementation of it

**Why it changed:** stage 3 needs nsjail. if the handler calls
exec directly, swapping in nsjail means touching the handler.
with the interface its just a new struct, callers dont change.
also made testing easier — can inject a fake runner in tests


## 2026-05-30 · build field in response

**What we thought we'd do:** always include a build key in the
response, empty object for python

**What we actually did:** build field uses a pointer with omitempty,
absent from response when nil

**Why it changed:** spec is explicit that the field should not
appear for interpreted languages. empty object would be wrong


## 2026-05-30 · top level status rule

**What we thought we'd do:** wasnt sure how to handle mixed test
results initially

**What we actually did:** top level is the first failing test in
order, stays accepted only if every test passes

**Why it changed:** tried worst-failure approach first but first
failure is more predictable for callers — you always know which
test caused it