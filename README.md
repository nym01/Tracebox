# goboxd

goboxd is an HTTP sandbox runner that accepts source code, compiles or interprets it inside a resource-limited subprocess, and returns stdout, stderr, exit code, and runtime metrics.

## Quick Start

The same setup script (`tracebox.sh` / `tracebox.ps1`) works two ways and
auto-detects which to use. Most people want the **standalone install** below;
contributors who want to build from source use the **clone & build** path.

In both cases the script starts the sandbox API, installs the `tracebox` CLI
onto your PATH, sets up the MCP server, and (if Claude Code is installed)
registers it. It is safe to re-run.

The only hard prerequisite is [Docker](https://docs.docker.com/get-docker/)
(Docker Desktop must be running). [Claude Code](https://docs.claude.com/en/docs/claude-code)
is optional — if present, the Tracebox MCP server is registered with it
automatically.

### Standalone install (no clone, no Go)

Downloads the prebuilt sandbox image from `ghcr.io/nym01/tracebox` and the
prebuilt CLI/MCP binaries from the GitHub release — **Docker is the only
requirement, no Go and no git clone needed.** One line:

```sh
# Linux / macOS
curl -fsSL https://raw.githubusercontent.com/nym01/Tracebox/main/tracebox.sh | sh
```

```powershell
# Windows (PowerShell)
irm https://raw.githubusercontent.com/nym01/Tracebox/main/tracebox.ps1 | iex
```

(Or download that one file and run it directly — `./tracebox.sh` /
`.\tracebox.ps1`.) The first run pulls the sandbox image, which can take a few
minutes.

### Clone & build (contributors)

Build everything from source. Needs [Docker](https://docs.docker.com/get-docker/),
[Go](https://go.dev/dl/), and a git checkout (with the nsjail submodule). Run the
script **from the repo root** — it detects the source tree and builds the image
(including nsjail) and the CLI/MCP binaries locally:

```sh
git clone --recurse-submodules https://github.com/nym01/Tracebox.git
cd Tracebox
./tracebox.sh       # Linux / macOS
```

```powershell
.\tracebox.ps1      # Windows (PowerShell)
```

The first run can take several minutes because it compiles nsjail from source.

### Then run any script in the sandbox

From any folder, regardless of which install path you used:

```sh
tracebox run script.py
```

Restart your terminal first if the script reported that it changed your PATH.

Manage the sandbox from anywhere — no need to re-run the setup script:

```sh
tracebox start            # start the sandbox (nsjail backend, default)
tracebox start --strict   # start with the gVisor backend (stronger isolation)
tracebox stop             # stop the sandbox
```

The code executes inside Tracebox's locked-down sandbox (no network, tight
CPU/memory limits, throwaway filesystem) — never on your own machine. See
[`cmd/tracebox-cli`](cmd/tracebox-cli) for all CLI options.

### Manual setup

Prefer to do it by hand? Start the API with `docker compose up -d --build`,
then run `./install.sh` (or `.\install.ps1`) to build and install just the CLI.

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
