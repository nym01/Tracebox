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

**Response summary:** a few options came up — using a
  validation library with struct tags, checking fields
  manually after decoding, or using a strict decoder that
  rejects unknown fields. went with manual validation,
  decode into a struct then check the fields yourself
  and return a 400 if something is wrong.

**What we used / didn't use:** used the manual struct
  approach, straightforward and no extra dependency needed.
  skipped the validator library, felt like too much for
  what is just a few field checks. didn't use
  DisallowUnknownFields either, not strict enough
  requirements to need that. also skipped the regex
  suggestion for the filename check, strings.Contains
  was simpler and did the job fine.






## 2026-05-29 · python execution

**Prompt:** how do i run a subprocess in go, pipe stdin
  and capture stdout with a timeout

**Response summary:** three ways came up — use
  exec.CommandContext with cmd.Output() and let the timeout
  kill the process automatically, manually pipe stdin and
  stdout with StdinPipe and StdoutPipe, or spin up goroutines
  for stdin, stdout and stderr separately with a select for
  timeout. went with the first one, set up the context
  timeout, wired stdin as a reader, and captured output
  directly.

**What we used / didn't use:** used exec.CommandContext
  with direct output capture, clean and simple. skipped
  the manual pipe approach, more control than needed here.
  skipped the goroutine based reader too, that makes sense
  for large streaming output but this is just capturing
  the result at the end. timeout kills the process
  automatically through the context so no extra handling
  needed for that either.




## 2026-05-29 · output comparison

**Prompt:** how to compare program output to expected in go

**Response summary:** explained string comparison and trimming
whitespace to tell apart wrong output vs whitespace mismatch

**What we used / didn't use:** used the approach as suggested,
worked cleanly for all three cases





## 2026-05-29 · runtime error and timeout

**Prompt:** how do i know if a process crashed or hit the
  time limit in go

**Response summary:** a few ways to handle it — check
  ctx.Err() after the process exits to catch timeouts,
  manually track a flag when you kill the process yourself,
  or map all outcomes into a clean status like Success,
  RuntimeError, Timeout, Killed. the simple version is
  just check ctx.Err() first, if it is DeadlineExceeded
  its a timeout, otherwise look at the exit code.

**What we used / didn't use:** went with context.WithTimeout
  and checked ctx.Err() after Wait. skipped the manual
  kill flag, no need to track that yourself when the
  context already handles process cleanup. skipped the
  full status layer too, maybe useful later but right now
  just two cases matter — did it timeout or did it crash.
  kept it to those two checks and it worked fine.




## 2026-05-29 · top level status rule

**Prompt:** how to make sure the top level status reflects
  the first failing test in go

**Response summary:** a few ways to do it — stop on the
  first failure and use that status, run all tests but
  track the first failure separately, or collect everything
  and pick the highest priority status like CompileError
  over Timeout over RuntimeError. the straightforward way
  is just iterate in order and only update the top level
  status while it is still accepted.

**What we used / didn't use:** logic was already correct,
  just iterated tests in order and stopped updating the
  top level once it was set to something other than
  accepted. skipped fail fast, wanted to still run all
  tests and collect results even after a failure. skipped
  the priority based approach too, adds complexity that
  isnt needed when order already gives you the right
  answer. just added a test to make sure the behavior
  stays that way.




## 2026-05-29 · c++ compilation

**Prompt:** how do i add a compile step in go before running
  the binary, and what happens if compilation fails

**Response summary:** a few ways to do it — build then run
  and return a compile error if it fails, build into a temp
  folder and clean up after, or treat compile and run as two
  separate phases with their own status. went with the
  straightforward one: run g++ as a subprocess, check the
  exit code, if it fails return build_failed and skip
  running anything.

**What we used / didn't use:** used the simple build then
  run approach. didn't bother with a temp directory, the
  binary just gets written to the working folder and that
  was fine. didn't need a full two phase pipeline either,
  the exit code check was enough to tell compile failed
  from runtime failed. also hit a docker port conflict
  while testing this, had nothing to do with the logic,
  just stopped the old container and moved on.







## 2026-05-29 · c++ error cases

**Prompt:** how to make sure all error statuses work for
  compiled languages the same way as interpreted ones

**Response summary:** looked at a few ways to handle this —
  running the same error tests across all languages, mapping
  each language failure into a common status like
  CompileError or Timeout, or keeping sample broken programs
  per language and checking them in CI. turned out none of
  that was needed. compiled languages use the same subprocess
  path, so the error handling already worked for c++ without
  any changes.

**What we used / didn't use:** just reused what was already
  there, no new code needed for c++. didn't add a status
  mapper because errors were already coming out consistent.
  didn't do the golden test matrix either, feels like
  something to add later when there are more languages.
  c++ just runs a compiled binary instead of calling an
  interpreter but the exit code handling is identical, so
  nothing broke.






## 2026-05-29 · stdin piping

**Prompt:** how to verify stdin is passed correctly to
  subprocesses in go

**Response summary:** options ranged from a simple echo
  test using cat or grep to bounce input back, to a
  dedicated test helper binary that reads and prints stdin,
  to a full pipe + integration test via cmd.StdinPipe().
  the straightforward path was strings.NewReader assigned
  to cmd.Stdin — no pipe management needed, just feed the
  reader and let the subprocess consume it.

**What we used / didn't use:** stdin wiring was already
  correct using strings.NewReader, so no change needed
  there. skipped the echo test approach — cat/grep works
  but tells you nothing about real language runtimes.
  skipped the dedicated helper binary too, unnecessary
  build artifact just for a pipe check. went with real
  language integration instead — tested with input() in
  python and cin in c++, which actually proves stdin
  works end to end for the languages that matter.





## 2026-05-29 · build field in response

**Prompt:** how to make a json field appear only for some
  languages and not others in go

**Response summary:** a few approaches exist — custom
  MarshalJSON() for full control over per-language field
  inclusion, separate DTO structs per language, or building
  a map[string]any and only adding fields when needed.
  the pointer + omitempty pattern covers the common case
  cleanly without any of that overhead — field is nil,
  it disappears from the json output.

**What we used / didn't use:** implementation was already
  in place using pointer + omitempty, so no structural
  change needed. skipped custom MarshalJSON — too much
  boilerplate for what is essentially a nil check. skipped
  separate DTOs too, that scales badly when you have many
  languages. map-based approach was tempting for flexibility
  but loses type safety. just added tests to confirm both
  cases — field present when set, absent when nil.








## 2026-05-29 · tests cleanup

**Prompt:** how to make sure all go packages have tests
  and everything is clean


**Response summary:** a few approaches came up — simple
  go test ./... + coverage check to catch zero-coverage
  packages, or scanning for missing *_test.go files per
  package, or a stricter gate with race detection and a
  coverage threshold. the lightweight path was enough here:
  go test ./... and go vet ./... to confirm everything
  compiles and passes cleanly.



**What we used / didn't use:** went with the simple run —
  go test ./... and go vet ./.... skipped the *_test.go
  file scanner (approach 2), felt like overkill when the
  test run itself already surfaces missing coverage.
  skipped race detection and the 80% threshold too — project
  isn't at that stage yet. added tests for language registry
  and runner where coverage was zero, everything passed clean
  after that.






## 2026-05-31 · timeout vs runtime_error misclassification

**Prompt:** how do i detect if a subprocess timed out vs
  crashed in go — both exit with non zero code

  **Response summary:** three approaches came up — use
  exec.CommandContext() with a deadline and check ctx.Err()
  after cmd.Run(), or check if the error itself wraps
  context.DeadlineExceeded, or manually track a killed flag
  before inspecting exit state. all three can work but the
  clean way is checking ctx.Err() == context.DeadlineExceeded
  right after Wait/Run returns, before touching the exit code.


**What we used / didn't use:** went with approach 1 —
  exec.CommandContext() + ctx.Err() check. approach 2 felt
  redundant once the context was already wired in. skipped
  approach 3 entirely, manual flag tracking adds state for no
  real benefit when the context already carries that info.
  one thing to watch: had to check context error *before*
  exit code or it still misclassified — order matters here.