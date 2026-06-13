# goboxd

goboxd is an HTTP sandbox runner that accepts source code, compiles or interprets it inside a resource-limited subprocess, and returns stdout, stderr, exit code, and runtime metrics.

## Quick Start

One command takes a fresh clone to a fully working setup: it starts the
sandbox API, builds and installs the `tracebox` CLI onto your PATH, builds the
MCP server, and (if Claude Code is installed) registers it.

**Prerequisites:**
- [Docker](https://docs.docker.com/get-docker/) — runs the sandbox API
  (Docker Desktop must be running)
- [Go](https://go.dev/dl/) — builds the CLI and MCP server
- _Optional:_ [Claude Code](https://docs.claude.com/en/docs/claude-code) — if
  present, the script registers the Tracebox MCP server with it automatically

**Run the setup script** from the repo root:

```sh
./tracebox.sh       # Linux / macOS
```

```powershell
.\tracebox.ps1      # Windows (PowerShell)
```

The script is safe to re-run — it skips work that is already done (containers
already up, CLI already installed, MCP already registered). The first run can
take a few minutes because it builds the sandbox image (including nsjail).

**Then run any script in the sandbox**, from any folder:

```sh
tracebox run script.py
```

Restart your terminal first if the script reported that it changed your PATH.

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
