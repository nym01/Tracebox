# tracebox-mcp

An [MCP](https://modelcontextprotocol.io) server that exposes the Tracebox
sandbox to AI agents (Claude Desktop, Claude Code, etc.) as a single tool,
`tracebox_run`. It is a **thin stdio client over the Tracebox HTTP API** — it does
no sandboxing itself and simply forwards each call to `POST /run` on a running
Tracebox API server.

The tool is named `tracebox_run` (not `run_code`) to avoid colliding with the
built-in `run_code` tool some clients (e.g. Claude Code) ship for notebook/IDE
execution, which would otherwise make the client ambiguous about which tool to
call.

## The `tracebox_run` tool

| Param      | Type   | Required | Description                                                        |
| ---------- | ------ | -------- | ------------------------------------------------------------------ |
| `language` | string | yes      | One of `py3`, `cpp`, `c`, `bash`, `js`, `java`, `verilog`.         |
| `code`     | string | yes      | The full source code to execute.                                   |
| `stdin`    | string | no       | Standard input fed to the program.                                 |

Returns a structured result: `run_id`, `status`, `stdout`, `stderr`,
`compile_output` (on build failure), `duration_ms`, and `memory_peak_kb`.
Output streams are truncated (tail kept) so responses stay concise.

For Java, the public class name is detected from the source and used as the
file name (falling back to `Main`), since `javac` requires a matching filename.

## Configuration

| Env var            | Default                 | Description                       |
| ------------------ | ----------------------- | --------------------------------- |
| `TRACEBOX_API_URL` | `http://localhost:8080` | Base URL of the Tracebox API.     |

## Running

First start the Tracebox API server (`cmd/tracebox`) so the endpoint is live,
then build and run the MCP server:

```sh
go build -o tracebox-mcp ./cmd/tracebox-mcp
TRACEBOX_API_URL=http://localhost:8080 ./tracebox-mcp
```

The server speaks MCP over stdio, so you normally don't run it by hand — an MCP
client launches it. To sanity-check that it builds and starts, run it and it
will log `serving over stdio`.

## Pointing a client at it

### Claude Code

```sh
claude mcp add tracebox --scope user --env TRACEBOX_API_URL=http://localhost:8080 -- /absolute/path/to/tracebox-mcp
```

The `--scope user` flag registers the server for your user account so it is
available from every directory. Without it, `claude mcp add` defaults to
`local` scope, which only registers the server for the current working
directory — it will then be missing whenever you launch Claude Code elsewhere.

### Claude Desktop

Add an entry under `mcpServers` in the config file
(`claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "tracebox": {
      "command": "/absolute/path/to/tracebox-mcp",
      "env": {
        "TRACEBOX_API_URL": "http://localhost:8080"
      }
    }
  }
}
```

Use an absolute path to the built binary. Restart the client after editing the
config; `tracebox_run` will then appear as an available tool.
