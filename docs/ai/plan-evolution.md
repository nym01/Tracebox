## 2026-05-27 · what I thought I was building at the start

**What we thought we'd do:**
when I first read the spec I thought this was basically
a simple API — receive code, run it, return output.
thought a few if/else checks for python and c++ would
be enough for stage 1. didn't realise how much was
involved in the "run it" part.

**What we actually did:**
Language struct with nil Build pointer, Runner interface,
proper temp dir cleanup, output comparison logic

**Why it changed:**
actually building it showed the gaps. the handler was
getting messy fast once c++ needed a compile step.


## 2026-05-28 · language registry instead of if/else

**What we thought we'd do:**
if/else in the handler — if python do this, if cpp do that

**What we actually did:**
Language struct with a nil Build pointer for interpreted
languages. handler just checks if Build is nil.

**Why it changed:**
once c++ needed a build step the handler was starting to
know too much about each language. adding a third language
would make it worse. struct keeps the handler clean.


## 2026-05-29 · Runner interface

**What we thought we'd do:**
call exec.Cmd directly in the handler, refactor later
if needed

**What we actually did:**
Runner interface from the start, SubprocessRunner as
the only implementation for stage 1

**Why it changed:**
stage 3 needs nsjail which is a completely different
execution backend. retrofitting an interface later
means rewriting the handler and all its tests.
adding it early costs almost nothing.


## 2026-05-29 · build field in response

**What we thought we'd do:**
always include a build key in the response, just empty
for python

**What we actually did:**
Build as a pointer with omitempty on the response struct

**Why it changed:**
omitempty on a struct value doesnt work — shows up as
{} anyway. pointer fixes it. also matches the spec —
build shouldnt appear for interpreted languages at all.