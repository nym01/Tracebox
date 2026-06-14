#!/usr/bin/env bash
# build-and-run.sh — EXPLORATORY. Reproduces the gVisor (runsc) feasibility probe.
#
# Phase 7 scoping POC. Touches NONE of the production code (internal/runner,
# nsjail.go, seccomp.policy, CLI, MCP, frontend). It only downloads runsc, builds
# a throwaway OCI bundle, and runs a handful of read-only checks to answer:
#   1. does runsc run AT ALL in this WSL2 + Docker Desktop environment (no KVM)?
#   2. can a minimal py3 "hello world" run under gVisor isolation?
#   3. does gVisor's synthesised /proc fix security-audit Finding F?
#   4. does runsc honour OCI memory/cpu/pids limits here?
#   5. (assessed in FINDINGS.md, demonstrated here) gVisor's own syscall tracing.
#
# It is meant to run INSIDE a privileged Linux container started with
# --cgroupns=host (the SAME deployment shape the nsjail sandbox already uses —
# see docker-compose.yml). From the repo root, on Windows/Git-Bash:
#
#   MSYS_NO_PATHCONV=1 docker run --rm --privileged --cgroupns=host \
#     -v "D:/PROJECTS/Tracebox/experiments/gvisor-poc:/poc" -w /poc \
#     python:3-slim bash /poc/build-and-run.sh
#
# A python:3-slim base is used so python3 + its shared libraries are already
# present to populate the OCI rootfs (see "build OCI bundle" below). The runsc
# binary, the OCI bundle and any logs land under /poc and are git-ignored.
set -uo pipefail
cd "$(dirname "$0")"

echo "== installing helpers (wget, jq) =="
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null 2>&1
apt-get install -y -qq --no-install-recommends wget ca-certificates jq >/dev/null 2>&1

echo "== downloading runsc (latest gVisor release) =="
ARCH=$(uname -m)
URL="https://storage.googleapis.com/gvisor/releases/release/latest/${ARCH}"
wget -q "${URL}/runsc" -O ./runsc && chmod +x ./runsc
export PATH="$PWD:$PATH"
runsc --version

echo "== environment probe (KVM? cgroup controllers?) =="
echo -n "  /dev/kvm: "; ls /dev/kvm >/dev/null 2>&1 && echo "present" || echo "ABSENT -> KVM platform unavailable, must use systrap"
echo    "  cgroup v2 controllers: $(cat /sys/fs/cgroup/cgroup.controllers 2>/dev/null)"
echo -n "  memory.swap.* at cg root: "; ls /sys/fs/cgroup/memory.swap.max >/dev/null 2>&1 && echo "present" || echo "ABSENT (no swap accounting in WSL2)"

# All runsc invocations use the systrap platform (no KVM) and no network.
R="runsc --platform=systrap --network=none"

echo ""
echo "############ 1. BASIC EXECUTION (systrap, no KVM) ############"
# runsc do uses the host filesystem as the rootfs — quickest possible smoke test.
$R --ignore-cgroups do echo HELLO_FROM_GVISOR 2>&1 | tail -3

echo ""
echo "############ 2. OCI BUNDLE + py3 hello world ############"
echo "== building OCI bundle rootfs from this python:3-slim container =="
rm -rf bundle && mkdir -p bundle/rootfs
# A gVisor container needs a root filesystem. The cheapest minimal py3 rootfs is
# the running container's own /bin /lib /usr /etc (python3 + every shared lib it
# loads are already there). ~250 MB; a purpose-built image could be far smaller.
tar -cf - --one-file-system -C / bin lib lib64 usr etc sbin 2>/dev/null \
  | tar -xf - -C bundle/rootfs 2>/dev/null
mkdir -p bundle/rootfs/proc bundle/rootfs/sys bundle/rootfs/dev bundle/rootfs/tmp
echo "  rootfs size: $(du -sh bundle/rootfs | cut -f1)"
printf 'import sys; print("PY", sys.version.split()[0], "under gVisor")\n' > bundle/rootfs/hello.py
( cd bundle && runsc spec )                 # generates a default OCI config.json
jq '.process.args=["python3","/hello.py"] | .process.terminal=false' \
   bundle/config.json > bundle/c && mv bundle/c bundle/config.json
# runsc reads config.json from the bundle dir; --bundle points it there.
$R --ignore-cgroups run --bundle bundle hello 2>&1 | tail -3

echo ""
echo "############ 3. FINDING F — /proc host-info leak ############"
cat > bundle/rootfs/procf.py <<'PYEOF'
def first(path, needle):
    for l in open(path):
        if needle in l: return l.strip()
    return needle+" <none>"
print("version :", open("/proc/version").read().strip())
print("cpuinfo :", first("/proc/cpuinfo","model name"))
print("meminfo :", first("/proc/meminfo","MemTotal"))
print("loadavg :", open("/proc/loadavg").read().strip())
import os; print("cpus    :", os.cpu_count())
PYEOF
echo "--- HOST values (what the nsjail sandbox leaks today) ---"
echo "  version : $(cat /proc/version)"
echo "  cpuinfo : $(grep -m1 'model name' /proc/cpuinfo)"
echo "  meminfo : $(grep -m1 MemTotal /proc/meminfo)"
echo "  loadavg : $(cat /proc/loadavg)"
echo "  cpus    : $(nproc)"
echo "--- gVisor values, NO limits (runsc do / --ignore-cgroups) ---"
jq '.process.args=["python3","/procf.py"] | .process.terminal=false' \
   bundle/config.json > bundle/c && mv bundle/c bundle/config.json
$R --ignore-cgroups run --bundle bundle procA 2>&1 | grep -E 'version|cpuinfo|meminfo|loadavg|cpus' || true
echo "--- gVisor values WITH a 256 MB / 0.5-core limit (cgroupns=host) ---"
jq '.linux.resources.memory={"limit":268435456,"swap":268435456}
    | .linux.resources.cpu={"quota":50000,"period":100000}
    | .linux.cgroupsPath="/runsc-procf"
    | .process.args=["python3","/procf.py"] | .process.terminal=false' \
   bundle/config.json > bundle/c && mv bundle/c bundle/config.json
$R run --bundle bundle procB 2>&1 | grep -E 'version|cpuinfo|meminfo|loadavg|cpus' || \
   echo "  (sandbox start may be flaky in this env — see FINDINGS.md; rerun if EOF)"

echo ""
echo "############ 4. RESOURCE LIMITS ############"
echo "== memory: 512 MB hard limit, guest tries to allocate 3 GB =="
printf 'b=bytes(10*1024*1024)\na=bytearray()\nfor i in range(300): a+=b\nprint("ALLOCATED",len(a)//1048576,"MB - NOT killed")\n' > bundle/rootfs/bomb.py
jq '.linux.resources.memory={"limit":536870912,"swap":536870912}
    | .linux.cgroupsPath="/runsc-bomb"
    | .process.args=["python3","/bomb.py"] | .process.terminal=false' \
   bundle/config.json > bundle/c && mv bundle/c bundle/config.json
# Retry past the intermittent "client sync file: EOF" sandbox-start failure so the
# OOM result (137) is demonstrated. Root cause (FINDINGS.md §4/§6): the sentry forks
# helper tasks at startup and fails with EAGAIN (newosproc / fork-exec "resource
# temporarily unavailable") when the host is short on thread/pid headroom — so even
# several retries may not succeed on a heavily-loaded WSL2 host. Expected to clear
# on a real Linux host with more headroom.
for attempt in 1 2 3 4 5; do
  $R run --bundle bundle bomb$attempt >/tmp/bomb.out 2>&1; ec=$?   # $? is runsc's, no pipe
  if grep -q "EOF" /tmp/bomb.out; then echo "  attempt $attempt: flaky start (EOF), retrying…"; continue; fi
  sed 's/^/  /' /tmp/bomb.out | tail -1
  echo "  exit=$ec  (137 == OOM SIGKILL == memory limit enforced)"; break
done

echo ""
echo "############ 5. gVisor's OWN syscall tracing (eBPF-tracer alternative) ############"
printf 'open("/etc/hostname").read(); open("/etc/os-release").read(); print("GUEST_DONE")\n' > bundle/rootfs/io.py
jq '.process.args=["python3","/io.py"] | .process.terminal=false' \
   bundle/config.json > bundle/c && mv bundle/c bundle/config.json
$R --ignore-cgroups --strace --debug --debug-log=./strace.log run --bundle bundle iorun 2>&1 | tail -1
echo "--- guest openat() events captured by runsc --strace (sentry-side, NOT host tracepoints) ---"
grep -aE 'openat\(' ./strace.log | grep -aE 'hostname|os-release|io.py' | head -4 || true

echo ""
echo "== done =="
