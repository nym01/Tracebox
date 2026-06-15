# Tracebox

Tracebox runs untrusted code — including AI-generated code — in a locked-down
sandbox, watches everything it does at the syscall level, and keeps a record
you can replay. Built mainly so you can let an AI agent run code it wrote
without worrying about what that code might actually do.

## Quick Start

The only requirement is [Docker](https://docs.docker.com/get-docker/) (with
Docker Desktop running). One command:

```sh
# Linux / macOS
curl -fsSL https://raw.githubusercontent.com/nym01/Tracebox/main/tracebox.sh | sh
```

```powershell
# Windows (PowerShell)
irm https://raw.githubusercontent.com/nym01/Tracebox/main/tracebox.ps1 | iex
```

This downloads the prebuilt sandbox image and the `tracebox` CLI, and sets
everything up. No Go, no git clone needed. The first run pulls the sandbox
image, which can take a few minutes depending on your connection.

If you have [Claude Code](https://docs.claude.com/en/docs/claude-code)
installed, the Tracebox MCP server is registered automatically — Claude Code
can then run code through Tracebox's sandbox using the `tracebox_run` tool.

### Run something

From any folder (restart your terminal first if it said your PATH changed):

```sh
tracebox run script.py
```

You'll get the program's output plus a plain-English summary of what it did.

### Manage the sandbox

```sh
tracebox start            # start the sandbox (default, fast)
tracebox start --strict   # start with gVisor (stronger isolation)
tracebox stop             # stop the sandbox
```

The code always runs inside Tracebox's sandbox — never on your own machine.

## How it's isolated

Two separate sandboxing backends, switchable at any time:

- **Default (nsjail)** — the sandboxed process runs on your real machine's
  kernel, but locked down with namespaces, a syscall filter, and resource
  limits. Fast (~40ms overhead).
- **`--strict` (gVisor)** — the sandboxed process talks to a userspace
  kernel reimplementation and never touches your real kernel at all.
  Stronger isolation, a bit slower (~200-250ms overhead).

Both modes are continuously checked against a suite of 21 escape tests —
real attempts to break out of the sandbox, all of which currently fail (as
they should). See [`docs/security.md`](docs/security.md) for the full
security model and [`docs/escape-tests.md`](docs/escape-tests.md) for the
test results.

## Building from source (contributors)

If you want to build everything yourself instead of using the prebuilt
image:

```sh
git clone --recurse-submodules https://github.com/nym01/Tracebox.git
cd Tracebox
./tracebox.sh       # Linux / macOS
```

```powershell
.\tracebox.ps1      # Windows (PowerShell)
```

Run from the repo root — the script detects the source tree and builds the
image (including compiling nsjail) and the CLI/MCP binaries locally instead
of downloading them. This takes several minutes the first time.

### Other useful commands

```sh
make test          # unit tests
make integration   # integration tests
make lint          # linter
```

## Web UI

A browser UI (run history + explanations) is available when building from
source:

```sh
cd web && npm install && npm run dev
```

Then open `http://localhost:5173`.

## Docs

- [`docs/security.md`](docs/security.md) — the security model and threat model
- [`docs/escape-tests.md`](docs/escape-tests.md) — the 21-test escape suite and results
- [`docs/decisions.md`](docs/decisions.md) — architecture decisions
- [`docs/gvisor-security-assessment.md`](docs/gvisor-security-assessment.md) — gVisor vs nsjail comparison