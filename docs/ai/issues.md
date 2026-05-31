## 2026-05-29 · docker port conflict on restart

**What we were trying to do:**
restart the container after adding c++ compilation to test it

**What went wrong:**
docker run failed saying port 8080 already in use. curl
was returning connection refused so thought the build broke.
spent time checking the dockerfile before realising the old
container was still running in the background.

**How we resolved it:**
docker stop on the old container, then reran. claude code
suggested docker ps first to check running containers which
helped.

**What we learned:**
always check docker ps before starting a new container,
or use --rm so it cleans up automatically.


## 2026-05-29 · timeout and runtime_error looked the same

**What we were trying to do:**
return time_exceeded when user code hits the wall time limit

**What went wrong:**
a timed out process exits with a non-zero code, same as a
crash. exit code alone couldnt tell them apart so everything
was coming back as runtime_error including timeouts.

**How we resolved it:**
check runCtx.Err() == context.DeadlineExceeded after Wait
returns. if context expired its a timeout regardless of exit
code. found this after asking claude how timeout detection
works in go.

**What we learned:**
check the context error before checking the exit code,
not after.


## 2026-05-29 · omitempty on struct doesnt work, needs pointer

**What we were trying to do:**
make the build field absent from python responses in json

**What went wrong:**
build field was showing up as {} in python responses even
with omitempty on the struct tag. didnt realise omitempty
only omits nil pointers not empty structs.

**How we resolved it:**
changed Build BuildResult to Build *BuildResult. nil pointer
gets omitted, non-nil gets serialized. claude explained why
omitempty behaves differently for structs vs pointers.

**What we learned:**
omitempty on a struct value never omits it, use a pointer.