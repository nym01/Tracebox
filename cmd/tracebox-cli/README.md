# tracebox-cli

A command-line tool that runs a source file in the **Tracebox sandbox** instead
of on your own machine:

```sh
tracebox run script.py
```

It reads the file, detects the language from its extension, sends the code to a
running Tracebox API server (`POST /run`), and prints the program's output with
a plain-English explanation of what happened.

Like [`tracebox-mcp`](../tracebox-mcp), this is a **thin client over the HTTP
`/run` endpoint** — it does no sandboxing itself. The API server
([`cmd/tracebox`](../tracebox)) must be running separately.

## Why

When an AI assistant hands you a script, the safe move is to try it somewhere
disposable before trusting it on your real machine. `tracebox run script.py`
does exactly that: the code executes inside Tracebox's locked-down sandbox
(no network, tight CPU/memory limits, throwaway filesystem), and you see what it
prints and whether it crashed — without it ever touching your environment.

## Build

Build with the output name `tracebox` so the command reads naturally:

```sh
# Linux / macOS
go build -o tracebox ./cmd/tracebox-cli

# Windows
go build -o tracebox.exe ./cmd/tracebox-cli
```

This produces a `tracebox` (`tracebox.exe` on Windows) binary. Put it on your
`PATH` to use `tracebox run` from anywhere.

The easiest way to do this is the install script at the repo root, which builds
the binary and sets up your `PATH` for you:

```sh
./install.sh          # Linux / macOS
```

```powershell
.\install.ps1         # Windows (PowerShell)
```

## Usage

```sh
tracebox run <file> [--stdin "input" | --stdin-file path]
tracebox start [--strict]
tracebox stop
```

`run` sends a source file to the API. `start` / `stop` bring the sandbox itself
up and down (see [Managing the sandbox](#managing-the-sandbox-start--stop)).

The language is detected from the file extension:

| Extension(s)            | Language ID |
| ----------------------- | ----------- |
| `.py`                   | `py3`       |
| `.cpp` `.cc` `.cxx`     | `cpp`       |
| `.c`                    | `c`         |
| `.sh`                   | `bash`      |
| `.js`                   | `js`        |
| `.java`                 | `java`      |
| `.v`                    | `verilog`   |

For Java, the public class name is detected from the source and used as the
file name (falling back to `Main`), since `javac` requires a matching filename.

### Examples

Run a Python script you got from an AI assistant, sandboxed:

```sh
tracebox run suspicious_cleanup.py
```

Feed it input on stdin:

```sh
tracebox run solve.py --stdin "3
1 2 3"
```

Feed input from a file:

```sh
tracebox run solve.cpp --stdin-file sample_input.txt
```

Point at a remote API server:

```sh
TRACEBOX_API_URL=https://tracebox.example.com tracebox run script.js
```

### Output

On a terminal the CLI prints, as applicable:

- A short status line that names the sandbox — e.g.
  `✓ ran successfully in Tracebox sandbox (no expected output provided)` or
  `✗ crashed in Tracebox sandbox`. The wording ("ran successfully", "crashed",
  "took too long", "used too much memory", "failed to compile", ...) matches the
  web UI (`web/src/explain.ts`) and the MCP server.
- The program's output in a bordered, colored box labeled `OUTPUT` (cyan); a
  separate red `STDERR` box below it when standard error is non-empty; and a red
  `COMPILE ERRORS` box on a build failure. Box content keeps the program's own
  colors — only the border and label are colored.
- A dimmed metadata line: `run_id`, the sandbox backend that executed the run
  (`nsjail` / `gvisor` / `subprocess`), the program's exit code, the run's
  duration, and peak memory when reported.

Pass `-v` / `--verbose` to also print the full plain-English explanation
paragraph above the output.

Colors and box-drawing are used only when stdout is an interactive terminal.
When output is piped to a file or another program, or when `NO_COLOR` is set (or
`TERM=dumb`), the CLI falls back to a plain-text layout — `=== OUTPUT ===` /
`=== STDERR ===` headers and a `key: value` metadata line — with no escape codes.

## Managing the sandbox (`start` / `stop`)

`tracebox run` talks to a sandbox API that must already be running. After the
initial setup (`tracebox.ps1` / `tracebox.sh`), you can start and stop that
sandbox from **any directory** — no need to re-run the full setup script or
`cd` back into the repo:

```sh
tracebox start            # start the sandbox (nsjail backend, the default)
tracebox start --strict   # start with the gVisor backend (stronger isolation, slower)
tracebox stop             # stop the sandbox
```

- **`start`** runs `docker compose up -d --build` and waits (up to 120s) for the
  API's `/healthz` and `/readyz` endpoints, then prints which mode it came up in.
- **`start --strict`** sets `GOBOXD_RUNNER=gvisor` so the sandbox uses the
  gVisor (runsc) backend — the stronger-isolation, slower mode. Without
  `--strict`, the default nsjail backend is used.
- **`stop`** runs `docker compose down`.

Both check that Docker is installed and running first, with a friendly error if
it isn't.

### How `start`/`stop` find the repo

The `tracebox` binary is on your `PATH` and has no built-in knowledge of where
the repo (and its `docker-compose.yml`) lives. The setup scripts record the
repo's absolute path in a small config file the first time you run them:

| OS            | Config file                       |
| ------------- | --------------------------------- |
| Windows       | `%USERPROFILE%\.tracebox\config`  |
| Linux / macOS | `~/.tracebox/config`              |

It is a plain `key=value` text file:

```
repo_path=/absolute/path/to/Tracebox
```

If this file is missing (the CLI was installed but the setup script was never
run here, or it ran on a different machine), `tracebox start` / `tracebox stop`
print a clear message telling you to run `tracebox.ps1` / `tracebox.sh` from the
repo once to create it.

## Configuration

| Env var            | Default                 | Description                   |
| ------------------ | ----------------------- | ----------------------------- |
| `TRACEBOX_API_URL` | `http://localhost:8080` | Base URL of the Tracebox API. |

## Exit codes

| Code | Meaning                                                                              |
| ---- | ------------------------------------------------------------------------------------ |
| `0`  | The sandboxed program **ran** — regardless of what it printed.                       |
| `1`  | The **run failed**: `build_failed`, `runtime_error`, `time_exceeded`, `memory_exceeded`, `internal_error`, `not_executed`. |
| `2`  | **CLI/usage error**: bad arguments, unknown file extension, or an unreadable file.   |
| `3`  | Could **not reach the API**, or the API returned an error.                           |

Exit `0` means "the program executed", not "the program succeeded at its task" —
the CLI supplies no expected output, so it can't judge correctness. Inspect the
printed stdout/stderr for that.
