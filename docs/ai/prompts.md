## 2026-05-29 · figuring out what stage 1 actually wants

**Prompt:** pasted the spec link and asked claude to break down
stage 1 in simple terms and give me a plan

**Response summary:** explained the core loop — get request,
validate it, build if needed, run tests, return result. said
python + c++ is the easiest combo to start with. broke it into
steps with docker verify at each stage.

**What we used / didn't use:** went with the python + c++ pick,
made sense to avoid java's filename mess for now. kept the runner
interface idea so nsjail isn't painful to add later. skipped the
yaml registry and nsjail for stage 1 — not needed yet.


## 2026-05-29 · setting up go project skeleton

**Prompt:** asked how to structure a go http server project

**Response summary:** explained net/http basics and go project
structure with cmd/ and internal/ folders

**What we used / didn't use:** used the folder structure and
dockerfile pattern. didn't use the suggested middleware setup,
kept it minimal for stage 1.


## 2026-05-29 · /run validation

**Prompt:** how do i validate incoming json fields in go
and send back proper error responses

**Response summary:** showed how to parse json into structs
and check fields manually

**What we used / didn't use:** used the struct approach, skipped
the regex suggestion for filename check, strings.Contains
was enough and simpler






## 2026-05-29 · python execution

**Prompt:** how do i run a subprocess in go, pipe stdin
and capture stdout with a timeout

**Response summary:** showed exec.Cmd with context timeout
and how to read process output

**What we used / didn't use:** used the subprocess approach,
skipped a goroutine based output reader suggestion, kept it
simpler with direct capture




## 2026-05-29 · output comparison

**Prompt:** how to compare program output to expected in go

**Response summary:** explained string comparison and trimming
whitespace to tell apart wrong output vs whitespace mismatch

**What we used / didn't use:** used the approach as suggested,
worked cleanly for all three cases


## 2026-05-29 · runtime error and timeout

**Prompt:** how do i know if a process crashed or hit the
time limit in go

**Response summary:** exit code for crashes, context timeout
for time limit. also suggested spawning a goroutine to kill
the process manually after timeout.

**What we used / didn't use:** went with exit code and
context.WithTimeout, the goroutine idea wasnt needed since
context already cleans up the process on its own




## 2026-05-29 · top level status rule

**Prompt:** how to make sure the top level status reflects
the first failing test in go

**Response summary:** explained iterating tests in order and
updating top level only when its still accepted

**What we used / didn't use:** logic was already correct from
earlier steps, just added a test to lock it down



## 2026-05-29 · c++ compilation

**Prompt:** how do i add a compile step in go before running
the binary, and what happens if compilation fails

**Response summary:** run g++ as a subprocess first, check
exit code, if it fails skip all tests and return build_failed

**What we used / didn't use:** used this exactly, also ran
into a docker port conflict while testing, fixed that
separately by stopping the old container


## 2026-05-29 · c++ error cases

**Prompt:** how to make sure all error statuses work for
compiled languages the same way as interpreted ones

**Response summary:** same subprocess approach works for
both, just runs the compiled binary instead of interpreter

**What we used / didn't use:** reused the existing error
handling, no new logic needed for c++ specifically





## 2026-05-29 · stdin piping

**Prompt:** how to verify stdin is passed correctly to
subprocesses in go

**Response summary:** explained that strings.NewReader pipes
stdin into the process, tested with input() in python and
cin in c++

**What we used / didn't use:** stdin was already wired
correctly, just added tests to confirm it works for both
languages





## 2026-05-29 · build field in response

**Prompt:** how to make a json field appear only for some
languages and not others in go

**Response summary:** explained using a pointer with omitempty
so the field is omitted when nil

**What we used / didn't use:** was already implemented
correctly, just added tests to confirm both cases




