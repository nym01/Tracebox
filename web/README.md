# Tracebox Web

A clean single-page UI for the [Tracebox](../) code-execution sandbox. Write
code in a Monaco editor, run it against the existing `/run` API, and get the raw
output plus a **plain-English explanation** of what happened — along with a
history of past runs.

It is a purely client-side frontend: no backend changes, no separate repo. It
talks directly to the Tracebox HTTP API from the browser (CORS is enabled
server-side).

## Stack

- React + Vite + TypeScript
- [`@monaco-editor/react`](https://github.com/suren-atoyan/monaco-react) for the
  code editor
- Plain CSS (dark theme), no UI framework

## Prerequisites

- Node.js 18+ and npm
- A running Tracebox API server. From the repo root:

  ```sh
  go run ./cmd/tracebox
  ```

  It listens on `http://localhost:8080` by default. CORS is already enabled
  (`internal/api/cors.go`), so the browser can call it directly.

## Setup & run

```sh
cd web
npm install
npm run dev
```

Then open the printed URL (default <http://localhost:5173>).

> The Monaco editor is loaded from a CDN at runtime by `@monaco-editor/react`,
> so the first editor render needs network access.

### Other scripts

| Command           | What it does                                  |
| ----------------- | --------------------------------------------- |
| `npm run dev`     | Start the Vite dev server with hot reload     |
| `npm run build`   | Type-check (`tsc -b`) and build to `dist/`    |
| `npm run preview` | Serve the production build locally            |

## Configuration

The API base URL is configurable via a Vite environment variable. The frontend
calls `${VITE_TRACEBOX_API_URL}/run`.

| Variable                 | Default                 | Description               |
| ------------------------ | ----------------------- | ------------------------- |
| `VITE_TRACEBOX_API_URL`  | `http://localhost:8080` | Base URL of Tracebox API  |

To override, copy the example file and edit it:

```sh
cp .env.example .env
# edit .env, e.g. VITE_TRACEBOX_API_URL=http://localhost:9000
```

Vite only exposes variables prefixed with `VITE_`, and `.env` is read at dev /
build start — restart the dev server after changing it.

## How it works

### Languages

The 7 supported languages mirror `configs/languages.yaml`: Python 3 (`py3`),
C++ (`cpp`), C (`c`), Bash (`bash`), JavaScript (`js`), Java (`java`), and
Verilog (`verilog`). Each maps to a Monaco editor mode and carries its
configured time / memory limits so explanations can name them.

### Request shape

Each run sends a single-test `POST /run` request matching `internal/api`:

```json
{
  "language": "py3",
  "source": "print('hi')",
  "tests": [{ "stdin": "", "expected_stdout": "" }]
}
```

Java additionally sets `source_filename` / `artifact_filename` from the public
class name (as `tracebox-mcp` does), since `javac` requires it.

### Plain-English explanation

The results panel maps each run status to a human explanation using rule-based
logic (no LLM), in `src/explain.ts`. It follows the same "ran vs. real failure"
remapping as `tracebox-mcp`'s `reportedStatus()`: because this UI never supplies
an expected output, the API's comparison verdicts are **not** pass/fail signals.

| API status                                                  | Explanation                                            |
| ----------------------------------------------------------- | ------------------------------------------------------ |
| `accepted`, `wrong_output`, `output_whitespace_mismatch`    | Your code ran successfully                              |
| `runtime_error`                                             | Your code crashed or was stopped by the sandbox (+stderr) |
| `time_exceeded`                                             | Your code took too long and was stopped (+time limit)  |
| `memory_exceeded`                                           | Your code used too much memory and was stopped (+limit)|
| `build_failed`                                              | Your code failed to compile (+compiler output)         |
| `internal_error`                                            | The sandbox hit an internal error                      |

### History

Past runs are persisted in browser `localStorage` (`tracebox.history.v1`, last
50 runs). Each entry stores the timestamp, language, a code snippet, the
plain-language status, and the `run_id`. Clicking an entry restores that code,
stdin, and result into the main view. "Clear" empties the history.

## Project layout

```
web/
├── index.html
├── src/
│   ├── main.tsx              # React entry
│   ├── App.tsx               # Layout + state
│   ├── api.ts                # /run client + request/response types
│   ├── languages.ts          # Language defs (mirror languages.yaml)
│   ├── explain.ts            # Rule-based status → plain English
│   ├── history.ts            # localStorage history
│   ├── index.css             # Dark theme
│   └── components/
│       ├── CodeEditor.tsx    # Monaco wrapper
│       ├── ResultPanel.tsx   # Explanation + outputs
│       └── HistoryPanel.tsx  # History sidebar
└── .env.example
```
