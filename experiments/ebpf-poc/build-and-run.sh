#!/usr/bin/env bash
# build-and-run.sh — EXPLORATORY. Builds the eBPF POC and runs a self-test.
#
# This is meant to run INSIDE a privileged Linux container that has the host
# kernel's BTF mounted at /sys/kernel/btf/vmlinux (true for Docker Desktop on
# WSL2). It installs the build toolchain, generates vmlinux.h, compiles the BPF
# object via bpf2go (clang), builds the Go binary, then traces a throwaway
# `python3` process by PID to prove file-open events are captured.
#
# Toolchain needed (documented here so it can move into a Dockerfile later):
#   clang llvm        — compile trace.bpf.c to a BPF ELF object
#   libbpf-dev        — <bpf/bpf_helpers.h> etc. used by trace.bpf.c
#   linux-libc-dev    — asm/types for the bpf headers
#   bpftool           — dump the kernel BTF to vmlinux.h (CO-RE)
#   golang            — build the loader (CGO not required: bpf2go embeds the .o)
set -euo pipefail
cd "$(dirname "$0")"

echo "== installing toolchain =="
export DEBIAN_FRONTEND=noninteractive
# Debian bookworm's `golang` apt package is 1.19 — too old for cilium/ebpf
# (needs 1.21+ for cmp/iter/maps/slices). Run this script on a golang:1.25 base
# so `go` is already present; only install the C/BPF toolchain via apt.
GO_PKG=""
command -v go >/dev/null 2>&1 || GO_PKG="golang"
apt-get update -qq
apt-get install -y -qq --no-install-recommends \
	clang llvm libbpf-dev linux-libc-dev bpftool ${GO_PKG} ca-certificates >/dev/null
go version

echo "== generating vmlinux.h from kernel BTF =="
bpftool btf dump file /sys/kernel/btf/vmlinux format c > vmlinux.h
wc -l vmlinux.h

echo "== go generate (bpf2go: clang compile + Go bindings) =="
go env -w GOFLAGS=-mod=mod
go generate ./...

echo "== go build =="
go build -o ebpf-poc .
ls -la ebpf-poc trace_bp* 2>/dev/null || true

# link.Tracepoint() attaches via tracefs; mount it if the container hasn't.
# (Privileged container only. For a non-privileged design this is a real
# constraint to revisit — see README.)
if [ ! -d /sys/kernel/tracing/events ] && [ ! -d /sys/kernel/debug/tracing/events ]; then
	echo "== mounting tracefs =="
	mount -t tracefs none /sys/kernel/tracing 2>/dev/null || \
		mount -t debugfs none /sys/kernel/debug 2>/dev/null || true
fi

echo "== self-test: trace a throwaway python3 by PID =="
# The tracer must be attached BEFORE the target does its opens (see FINDINGS.md
# on the attach race), so the target sleeps 3s while we grab its PID and attach.
# A second "noise" python3 runs concurrently to prove the filter excludes it.
python3 -u -c '
import time, sys
time.sleep(3)                 # give the tracer time to attach first
open("/etc/hostname").read()  # representative explicit opens
open("/etc/os-release").read()
open("/proc/self/status").read()
import hashlib, ssl           # force some .so / stdlib opens
sys.stdout.write("TARGET-done\n")
' &
CHILD=$!
python3 -c 'import time; time.sleep(3); open("/etc/passwd").read()' &
NOISE=$!
echo "target pid=$CHILD   noise pid=$NOISE (noise must NOT appear)"
timeout 6 ./ebpf-poc "$CHILD" || true
wait 2>/dev/null || true
echo "== done =="
