# goboxd

goboxd is an HTTP sandbox runner that accepts source code, compiles or interprets it inside a resource-limited subprocess, and returns stdout, stderr, exit code, and runtime metrics.

## Quick Start

Get from a fresh clone to running a script in the sandbox in three steps.

**Prerequisites:** [Docker](https://docs.docker.com/get-docker/) (runs the
sandbox API) and [Go](https://go.dev/dl/) (builds the CLI).

**1. Start the sandbox API** (builds the image and runs it in the background):

```sh
docker compose up -d --build
```

**2. Install the CLI** (one-time setup — builds `tracebox` and puts it on your PATH):

```sh
./install.sh        # Linux / macOS
```

```powershell
.\install.ps1       # Windows (PowerShell)
```

Restart your terminal afterward if the script changed your PATH.

**3. Run any script in the sandbox**, from any folder:

```sh
tracebox run script.py
```

The code executes inside Tracebox's locked-down sandbox (no network, tight
CPU/memory limits, throwaway filesystem) — never on your own machine. See
[`cmd/tracebox-cli`](cmd/tracebox-cli) for all CLI options.

## Framework

The server uses `net/http` from the Go standard library. It covers everything the spec requires without pulling in a third-party dependency, and the 1.22 method-based routing (`GET /healthz`, `POST /run`) removes the only reason to reach for a router.

## Running

Start the service with Docker Compose (this enables the nsjail sandbox and the
`--privileged` / host-cgroup access it needs — see below):

```
docker compose up
```

Or run the image directly. The nsjail sandbox requires `--privileged` (to create
namespaces, apply the seccomp filter and bind mounts) and `--cgroupns=host` (so
nsjail can reach the host cgroup v2 hierarchy to enforce the per-language memory
limit; without it, memory limits are not enforced and a memory bomb would not be
reported as `memory_exceeded`):

```
docker run --privileged --cgroupns=host --rm -p 8080:8080 -e GOBOXD_RUNNER=nsjail tracebox
```

The host must use **cgroup v2** (the unified hierarchy mounted at
`/sys/fs/cgroup`). nsjail selects the v2 backend via `--use_cgroupv2` and writes
`memory.max` for each request. Resident memory — not virtual address space — is
capped, so the JVM and V8 run normally while still being OOM-killed if their real
footprint exceeds the language's `memory_kb` budget.

All other operations go through the Makefile:

| Target           | What it does                          |
|------------------|---------------------------------------|
| `make build`     | Compile the binary                    |
| `make run`       | Build and start the server            |
| `make test`      | Run unit tests                        |
| `make integration` | Run integration tests              |
| `make lint`      | Run the linter                        |

## Docs

Architecture decisions and development notes are in `docs/ai/`.
