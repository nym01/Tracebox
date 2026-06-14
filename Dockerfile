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
# shared py3 rootfs at the path internal/runner/gvisor.go expects by default.
COPY --from=runsc-downloader /usr/local/bin/runsc /usr/local/bin/runsc
COPY --from=gvisor-py3-rootfs /rootfs /opt/gvisor/rootfs/py3
COPY configs/ configs/
EXPOSE 8080
CMD ["./goboxd"]
