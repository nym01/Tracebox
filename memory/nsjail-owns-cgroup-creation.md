---
name: nsjail-owns-cgroup-creation
description: nsjail creates the per-request cgroup itself (NSJAIL.<pid>); goboxd cannot pre-know the cgroup id before spawn
metadata:
  type: project
---

When goboxd passes `--cgroup_mem_max`/`--cgroup_pids_max`/`--cgroup_cpu_ms_per_sec`, **nsjail creates the cgroup itself** — `external/nsjail/cgroup2.cc:56` (`getCgroupPath`) names it `<cgroupv2_mount>/NSJAIL.<pid>` where `<pid>` is the clone-child PID that nsjail only learns *after* fork (`subproc.cc` `runChild` → `initParent` → `cgroup2::initNsFromParent`). The child is added to `cgroup.procs` *before* it execs (parent signals via socketpair only after cgroup setup), and the cgroup is removed on reap (`finishFromParent`). nsjail exposes **no flag** to use a pre-created/explicit leaf cgroup (only `--cgroupv2_mount` sets the base).

Consequence for Phase 4 eBPF tracing: the POC's `experiments/ebpf-poc/FINDINGS.md` premise that "goboxd creates that cgroup, so its id is known before the child executes" is **wrong**. Race-free *before-spawn* cgroup-id filtering would require patching vendored nsjail C++, which risks the Findings A-C cgroup-limit behavior. So Phase 4 v1 uses **after-spawn cgroup-id discovery**: find the child via `/proc/<nsjailpid>/task/<nsjailpid>/children`, read `/proc/<child>/cgroup`, stat `/sys/fs/cgroup<path>` whose inode == `bpf_get_current_cgroup_id()`. Documented v1 limitation: opens before discovery completes are missed. See [[phase4-tracer-design]].
