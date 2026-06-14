# Memory index

- [nsjail owns cgroup creation](nsjail-owns-cgroup-creation.md) — nsjail makes the per-request cgroup (NSJAIL.<pid>) itself; goboxd can't pre-know the id before spawn
- [Phase 4 eBPF tracer design](phase4-tracer-design.md) — internal/tracer file-open (4a) + exec/process-spawn (4b) monitor: cgroup-id-after-spawn filtering, persistent attach, path+argv exec capture, v1 limits
