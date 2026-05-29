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



