## 2026-05-29 · docker port conflict

**What we were trying to do:**
test c++ after adding compilation, restarted docker

**What went wrong:**
docker run just failed. curl wasnt connecting so i
thought i broke something in the code. checked the
dockerfile, checked the go files, wasted like 20
minutes before realising the old container was still
running from before and holding the port.

**How we resolved it:**
docker ps showed the old container, stopped it, worked
fine after that. felt stupid honestly but the error
message said port in use not "old container still up"
so wasnt obvious.

**What we learned:**
check docker ps first or just use --rm so containers
die automatically when you stop them.


## 2026-05-29 · timeout was coming back as runtime_error

**What we were trying to do:**
make infinite loop return time_exceeded not runtime_error

**What went wrong:**
both timeout and crash exit with non zero code so i
couldnt tell them apart. everything was runtime_error
even actual timeouts. had no idea how to detect which
one it was.

**How we resolved it:**
asked claude how to detect timeout in go subprocess.
turned out you check runCtx.Err() after Wait returns,
if its context.DeadlineExceeded its a timeout. exit
code doesnt matter. had to check context error before
checking exit code otherwise it still misclassifies.

**What we learned:**
cant rely on exit code alone, check context error first.


## 2026-05-29 · omitempty wasnt working on build field

**What we were trying to do:**
hide the build field from python responses, spec says
it shouldnt be there for interpreted languages

**What went wrong:**
added omitempty to the tag, build was still showing
up as {} in the response. tried a few things, nothing
worked, was confused for a while.

**How we resolved it:**
asked claude why omitempty wasnt working. turns out
omitempty doesnt omit empty structs only nil pointers.
changed Build to a pointer, nil gets omitted, done.
wish i knew this earlier wouldve saved time.

**What we learned:**
omitempty on a struct does nothing, needs to be
a pointer.