# Memory index

- [nsjail owns cgroup creation](nsjail-owns-cgroup-creation.md) — nsjail makes the per-request cgroup (NSJAIL.<pid>) itself; goboxd can't pre-know the id before spawn
- [Phase 4 eBPF tracer design](phase4-tracer-design.md) — internal/tracer file-open monitor: cgroup-id-after-spawn filtering, persistent attach, v1 limits
