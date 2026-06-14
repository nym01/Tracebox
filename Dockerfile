FROM golang:1.25 AS builder
WORKDIR /app
# eBPF toolchain for the Phase 4 file-open tracer: clang/llvm compile
# internal/tracer/trace.bpf.c to a BPF ELF object, libbpf-dev provides
# <bpf/bpf_helpers.h>, linux-libc-dev provides the linux uapi headers. bpftool is
# not needed: the BPF program uses only stable tracepoint fields + helpers, so it
# requires no generated vmlinux.h. CGO stays disabled — bpf2go embeds the .o.
RUN apt-get update && \
    apt-get install -y --no-install-recommends clang llvm libbpf-dev linux-libc-dev && \
    rm -rf /var/lib/apt/lists/*
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Generate the bpf2go bindings + embedded object from trace.bpf.c, then build.
RUN go generate ./internal/tracer/ && \
    CGO_ENABLED=0 GOOS=linux go build -o goboxd ./cmd/tracebox

# Build nsjail from source (pinned to tag 3.4 via the external/nsjail submodule).
# Do NOT bundle a prebuilt binary and do NOT install nsjail from apt — it must
# be compiled at image-build time. This stage takes several minutes.
FROM debian:bookworm-slim AS nsjail-builder
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        autoconf bison flex gcc g++ git libprotobuf-dev \
        libnl-route-3-dev libtool make pkg-config protobuf-compiler && \
    rm -rf /var/lib/apt/lists/*
COPY external/nsjail /build/nsjail
RUN cd /build/nsjail && make && cp nsjail /usr/local/bin/nsjail

# Phase 7 (gVisor backend, py3-only) — both stages below are ADDITIVE: they are
# only consumed when GOBOXD_RUNNER=gvisor and do not touch the nsjail/subprocess
# paths. See experiments/gvisor-poc/FINDINGS.md and internal/runner/gvisor.go.

# Download the runsc (gVisor) binary. Pinned to the release the POC validated
# (release-20260608.0). runsc is a single static binary — no daemon, no kernel
# module — so it is simply fetched and made executable, then copied into the final
# image. The release bucket path is keyed by the release date (the date portion of
# the release-YYYYMMDD.N tag).
FROM debian:bookworm-slim AS runsc-downloader
ARG RUNSC_RELEASE=20260608
RUN apt-get update && \
    apt-get install -y --no-install-recommends wget ca-certificates && \
    rm -rf /var/lib/apt/lists/*
RUN set -eux; \
    ARCH="$(uname -m)"; \
    BASE="https://storage.googleapis.com/gvisor/releases/release/${RUNSC_RELEASE}/${ARCH}"; \
    wget -q "${BASE}/runsc" -O /usr/local/bin/runsc; \
    chmod 0755 /usr/local/bin/runsc; \
    /usr/local/bin/runsc --version

# Build the shared, read-only python3 rootfs the GvisorRunner runs every py3 request
# against. gVisor needs a populated rootfs tree (unlike nsjail's per-file bind
# mounts), so the POC populated one by tar-ing a python container's userland. Here a
# dedicated debian-slim + apt python3 stage is used so the rootfs is python-only
# (~150 MB, not the whole multi-language runtime image) AND so python3 lands at
# /usr/bin/python3 — the exact path configs/languages.yaml invokes (a python:3-slim
# base would put it under /usr/local/bin and break that path). cp -a preserves the
# usr-merge symlinks (/bin, /lib, /lib64 -> /usr/*). The empty proc/sys/dev/tmp/work
# dirs are the mount points config.json binds at runtime. A purpose-built minimal
# rootfs (just python3 + its ldd/stdlib closure) is a future optimization; this
# whole-userland copy is the POC's validated approach.
FROM debian:bookworm-slim AS gvisor-py3-rootfs
RUN apt-get update && \
    apt-get install -y --no-install-recommends python3 && \
    rm -rf /var/lib/apt/lists/*
RUN set -eux; \
    mkdir -p /rootfs; \
    for d in bin lib lib64 usr etc sbin; do \
        if [ -e "/$d" ]; then cp -a "/$d" /rootfs/; fi; \
    done; \
    mkdir -p /rootfs/proc /rootfs/sys /rootfs/dev /rootfs/tmp /rootfs/work; \
    test -e /rootfs/usr/bin/python3

# Stage 2 (all 7 languages): one rootfs per language, each carrying everything that
# language needs for BOTH its build and run steps. This is deliberate — unlike
# nsjail (which gives the compiler build step broad filesystem access but the
# compiled artifact's run step a minimal one), under gVisor the sentry mediates
# every guest syscall, so rootfs minimalism is not the security boundary the way a
# minimal host mount is for nsjail. A static, read-only, purpose-built tree that the
# guest cannot escape (network=none, read-only root, sentry-blocked syscalls) is
# sound for both phases; see the GvisorRunner doc comment in internal/runner/gvisor.go
# for the full reasoning. Every stage mirrors the gvisor-py3-rootfs pattern: apt the
# language's userland into a debian-slim, cp -a the merged-usr tree, create the
# empty mount points, and assert the binaries the registry invokes are present.

# bash: the interpreter and its shared libraries (bash is in the base image; apt is
# explicit so a base change cannot silently drop it). No external tools are staged —
# a script that shells out to an unbound command simply will not find it, same as
# the nsjail bash profile.
FROM debian:bookworm-slim AS gvisor-bash-rootfs
RUN apt-get update && \
    apt-get install -y --no-install-recommends bash && \
    rm -rf /var/lib/apt/lists/*
RUN set -eux; \
    mkdir -p /rootfs; \
    for d in bin lib lib64 usr etc sbin; do \
        if [ -e "/$d" ]; then cp -a "/$d" /rootfs/; fi; \
    done; \
    mkdir -p /rootfs/proc /rootfs/sys /rootfs/dev /rootfs/tmp /rootfs/work; \
    test -e /rootfs/usr/bin/bash

# js: nodejs plus the externalized-builtins directory (/usr/share/nodejs, under usr,
# carried by the usr copy) node needs at startup. The current Debian package ships
# the real binary at /usr/bin/node (with /usr/bin/nodejs -> node), so the cp -a
# already places /usr/bin/node — the path the registry invokes. Only synthesise the
# node symlink if a package layout ever ships just /usr/bin/nodejs, and as a relative
# link so it resolves inside the rootfs (never clobber an existing real binary, or it
# becomes a node -> nodejs -> node loop).
FROM debian:bookworm-slim AS gvisor-js-rootfs
RUN apt-get update && \
    apt-get install -y --no-install-recommends nodejs && \
    rm -rf /var/lib/apt/lists/*
RUN set -eux; \
    mkdir -p /rootfs; \
    for d in bin lib lib64 usr etc sbin; do \
        if [ -e "/$d" ]; then cp -a "/$d" /rootfs/; fi; \
    done; \
    if [ ! -e /rootfs/usr/bin/node ]; then ln -sf nodejs /rootfs/usr/bin/node; fi; \
    mkdir -p /rootfs/proc /rootfs/sys /rootfs/dev /rootfs/tmp /rootfs/work; \
    test -e /rootfs/usr/bin/node && test -d /rootfs/usr/share/nodejs

# ccpp: one rootfs for BOTH c and cpp, BOTH build and run. g++ pulls in gcc, the
# C/C++ headers, binutils (as/ld the drivers shell out to) and the libstdc++/libgcc_s
# runtime a compiled binary needs — so this single tree serves the gcc build, the g++
# build, and the "./solution" run for either language. (A C binary's library needs
# are a subset of a C++ one's, exactly why nsjail already shares their run profile.)
FROM debian:bookworm-slim AS gvisor-ccpp-rootfs
RUN apt-get update && \
    apt-get install -y --no-install-recommends gcc g++ && \
    rm -rf /var/lib/apt/lists/*
RUN set -eux; \
    mkdir -p /rootfs; \
    for d in bin lib lib64 usr etc sbin; do \
        if [ -e "/$d" ]; then cp -a "/$d" /rootfs/; fi; \
    done; \
    mkdir -p /rootfs/proc /rootfs/sys /rootfs/dev /rootfs/tmp /rootfs/work; \
    test -e /rootfs/usr/bin/gcc && test -e /rootfs/usr/bin/g++

# java: the full JDK (javac + java). The whole JAVA_HOME tree (under /usr/lib/jvm,
# carried by the usr copy) plus the Debian /etc/java-*-openjdk config (carried by the
# etc copy) come along. No launcher-symlink surgery: the JDK derives JAVA_HOME from
# /proc/self/exe, and the update-alternatives symlink chain (/usr/bin/java ->
# /etc/alternatives/java -> /usr/lib/jvm/.../bin/java) is preserved by cp -a and
# resolves inside the rootfs at run time, so the native derivation just works.
FROM debian:bookworm-slim AS gvisor-java-rootfs
RUN apt-get update && \
    apt-get install -y --no-install-recommends default-jdk-headless && \
    rm -rf /var/lib/apt/lists/*
RUN set -eux; \
    mkdir -p /rootfs; \
    for d in bin lib lib64 usr etc sbin; do \
        if [ -e "/$d" ]; then cp -a "/$d" /rootfs/; fi; \
    done; \
    mkdir -p /rootfs/proc /rootfs/sys /rootfs/dev /rootfs/tmp /rootfs/work; \
    test -e /rootfs/usr/bin/javac && test -e /rootfs/usr/bin/java

# verilog: iverilog (build driver) + vvp (run), plus the Icarus "ivl base directory"
# (the ivl/ivlpp helpers and the *.tgt/*.vpi modules, under /usr/lib, carried by the
# usr copy). /bin/sh is present (base image) — the iverilog driver needs it for its
# system() sub-invocations. NOTE: unlike nsjail (which omits the shell from vvp's run
# profile so untrusted Verilog cannot reach a shell via $system()), this single
# rootfs leaves /bin/sh reachable from the run step too. That is the assessed
# trade-off of the one-rootfs model: a shell spawned by $system() runs inside the
# sentry with no network, a read-only root and the escape-class syscalls blocked, so
# it grants no escalation — see the GvisorRunner doc comment.
FROM debian:bookworm-slim AS gvisor-verilog-rootfs
RUN apt-get update && \
    apt-get install -y --no-install-recommends iverilog && \
    rm -rf /var/lib/apt/lists/*
RUN set -eux; \
    mkdir -p /rootfs; \
    for d in bin lib lib64 usr etc sbin; do \
        if [ -e "/$d" ]; then cp -a "/$d" /rootfs/; fi; \
    done; \
    mkdir -p /rootfs/proc /rootfs/sys /rootfs/dev /rootfs/tmp /rootfs/work; \
    test -e /rootfs/usr/bin/iverilog && test -e /rootfs/usr/bin/vvp

FROM debian:bookworm-slim
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        python3 g++ gcc bash nodejs default-jdk-headless iverilog \
        libprotobuf32 libnl-route-3-200 libnl-3-200 && \
    ln -sf /usr/bin/nodejs /usr/bin/node 2>/dev/null || true && \
    rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/goboxd .
COPY --from=nsjail-builder /usr/local/bin/nsjail /usr/local/bin/nsjail
# gVisor backend (consumed only when GOBOXD_RUNNER=gvisor): the runsc binary and the
# per-language rootfs trees under the base dir internal/runner/gvisor.go expects by
# default (/opt/gvisor/rootfs/<name>). c and cpp share the single ccpp tree.
COPY --from=runsc-downloader /usr/local/bin/runsc /usr/local/bin/runsc
COPY --from=gvisor-py3-rootfs /rootfs /opt/gvisor/rootfs/py3
COPY --from=gvisor-bash-rootfs /rootfs /opt/gvisor/rootfs/bash
COPY --from=gvisor-js-rootfs /rootfs /opt/gvisor/rootfs/js
COPY --from=gvisor-ccpp-rootfs /rootfs /opt/gvisor/rootfs/ccpp
COPY --from=gvisor-java-rootfs /rootfs /opt/gvisor/rootfs/java
COPY --from=gvisor-verilog-rootfs /rootfs /opt/gvisor/rootfs/verilog
COPY configs/ configs/
EXPOSE 8080
CMD ["./goboxd"]
