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

```sh
go build -o tracebox ./cmd/tracebox-cli
```

This produces a `tracebox` binary. Put it on your `PATH` to use `tracebox run`
from anywhere.

## Usage

```sh
tracebox run <file> [--stdin "input" | --stdin-file path]
```

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

The CLI prints, as applicable:

- `=== compile output ===` — the compiler's errors (on build failure).
- `=== stdout ===` / `=== stderr ===` — the program's output streams.
- A one-line plain-English explanation ("ran successfully", "crashed",
  "took too long", "used too much memory", "failed to compile", ...).
- The `run_id` and the run's duration (and peak memory when reported).

The explanation wording matches the web UI (`web/src/explain.ts`) and the MCP
server, so all three describe results the same way.

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
