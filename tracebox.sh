#!/usr/bin/env sh
# tracebox.sh — one-command setup for Tracebox (Linux / macOS).
#
# Takes a fresh clone to a fully working setup:
#   1. Checks Docker is installed and running.
#   2. Checks Go is installed.
#   3. Starts the sandbox API with docker compose and waits until it is healthy.
#   4. Builds and installs the tracebox CLI onto your PATH.
#   5. Builds the tracebox-mcp MCP server.
#   6. Registers the MCP server with Claude Code (if the `claude` CLI is found).
#   7. Records the repo location so `tracebox start`/`tracebox stop` work anywhere.
#   8. Prints a summary of what was set up.
#
# Safe to re-run: existing containers, an already-installed CLI and an
# already-registered MCP server are detected and left alone / updated.

set -eu

# --- Locate the repo root (the directory containing this script). ----------
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$SCRIPT_DIR"

API_URL="http://localhost:8080"
CLI_BINARY="tracebox"
MCP_BINARY="tracebox-mcp"

# --- Pretty output helpers. ------------------------------------------------
if [ -t 1 ]; then
    C_CYAN=$(printf '\033[36m'); C_GREEN=$(printf '\033[32m')
    C_YELLOW=$(printf '\033[33m'); C_RED=$(printf '\033[31m'); C_OFF=$(printf '\033[0m')
else
    C_CYAN=''; C_GREEN=''; C_YELLOW=''; C_RED=''; C_OFF=''
fi
step() { printf '\n%s[%s/8] %s%s\n' "$C_CYAN" "$1" "$2" "$C_OFF"; }
ok()   { printf '%s  OK  %s%s\n' "$C_GREEN" "$1" "$C_OFF"; }
info() { printf '      %s\n' "$1"; }
warn() { printf '%s  !!  %s%s\n' "$C_YELLOW" "$1" "$C_OFF"; }
fail() { printf '\n%sERROR: %s%s\n' "$C_RED" "$1" "$C_OFF"; exit 1; }

printf 'Tracebox setup\n'
printf 'Repo: %s\n' "$SCRIPT_DIR"

# --- 1. Docker installed and running. --------------------------------------
step 1 "Checking Docker..."
if ! command -v docker >/dev/null 2>&1; then
    fail "Docker not found - install from https://docker.com/get-started,
       make sure Docker Desktop is running, then re-run this script."
fi
if ! docker info >/dev/null 2>&1; then
    fail "Docker is installed but the daemon is not responding.
       Start Docker (Desktop), wait for it to finish starting, then re-run this script."
fi
ok "Docker is installed and running."

# --- 2. Go installed. ------------------------------------------------------
step 2 "Checking Go..."
if ! command -v go >/dev/null 2>&1; then
    fail "Go not found - install from https://go.dev, then re-run this script.
       (Pre-built binaries may be offered as an alternative in a future release.)"
fi
ok "Go is installed ($(go version))."

# --- 3. Start the sandbox API and wait for it to be healthy. ---------------
step 3 "Starting the sandbox API (docker compose up -d --build)..."
info "First build compiles nsjail and can take a few minutes - please wait."
if ! docker compose up -d --build; then
    fail "docker compose failed to start the sandbox API. See the output above."
fi
info "Containers are up. Waiting for the API to become healthy..."

# Pick whichever HTTP client is available.
http_status() {
    # echoes the HTTP status code (or empty on failure) for URL $1
    if command -v curl >/dev/null 2>&1; then
        curl -s -o /dev/null -m 5 -w '%{http_code}' "$1" 2>/dev/null || true
    elif command -v wget >/dev/null 2>&1; then
        if wget -q -T 5 -O /dev/null "$1" 2>/dev/null; then echo 200; else echo ""; fi
    else
        fail "Neither curl nor wget is available to probe the API health endpoints."
    fi
}

TIMEOUT_SEC=120
elapsed=0
health_ok=0
ready_ok=0
while [ "$elapsed" -lt "$TIMEOUT_SEC" ]; do
    if [ "$health_ok" -eq 0 ]; then
        if [ "$(http_status "$API_URL/healthz")" = "200" ]; then
            health_ok=1; ok "/healthz is up."
        fi
    fi
    if [ "$health_ok" -eq 1 ] && [ "$ready_ok" -eq 0 ]; then
        if [ "$(http_status "$API_URL/readyz")" = "200" ]; then
            ready_ok=1; ok "/readyz reports ready."
        fi
    fi
    [ "$health_ok" -eq 1 ] && [ "$ready_ok" -eq 1 ] && break
    sleep 3
    elapsed=$((elapsed + 3))
done

if [ "$health_ok" -eq 0 ]; then
    warn "The API did not pass /healthz within ${TIMEOUT_SEC} seconds."
    info "Check the logs with:  docker compose logs"
    fail "Sandbox API is not healthy."
fi
if [ "$ready_ok" -eq 0 ]; then
    warn "/healthz is up but /readyz is not fully ready (a language probe may be degraded)."
    info "Continuing anyway - check 'docker compose logs' if runs fail. Details: $API_URL/readyz"
else
    ok "Sandbox API is healthy at $API_URL"
fi

# --- 4. Build and install the CLI. -----------------------------------------
step 4 "Building and installing the tracebox CLI..."
go build -o "$CLI_BINARY" ./cmd/tracebox-cli
info "Built ./$CLI_BINARY"

SRC="$SCRIPT_DIR/$CLI_BINARY"
CLI_DEST=""
PATH_HINT=0

try_install() {
    dest_dir="$1"
    if [ -d "$dest_dir" ] && [ -w "$dest_dir" ]; then
        cp "$SRC" "$dest_dir/$CLI_BINARY"
        chmod +x "$dest_dir/$CLI_BINARY"
        CLI_DEST="$dest_dir/$CLI_BINARY"
        return 0
    fi
    return 1
}

if try_install "/usr/local/bin"; then :
elif mkdir -p "$HOME/.local/bin" 2>/dev/null && try_install "$HOME/.local/bin"; then :
else
    warn "Could not copy $CLI_BINARY to a directory on your PATH automatically."
    info "Finish with sudo:   sudo cp \"$SRC\" /usr/local/bin/$CLI_BINARY"
    info "or add the repo to your PATH (in ~/.bashrc or ~/.zshrc):"
    info "    export PATH=\"$SCRIPT_DIR:\$PATH\""
    CLI_DEST="$SRC"
fi
ok "Installed CLI: $CLI_DEST"

# Warn if the chosen dir isn't actually on PATH yet (common for ~/.local/bin).
INSTALL_DIR=$(dirname "$CLI_DEST")
case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *)
        PATH_HINT=1
        warn "$INSTALL_DIR is not on your PATH yet. Add it (in ~/.bashrc or ~/.zshrc):"
        info "    export PATH=\"$INSTALL_DIR:\$PATH\""
        ;;
esac

# Confirm the binary is callable.
if "$CLI_DEST" --help >/dev/null 2>&1; then
    ok "tracebox is callable."
else
    warn "Could not invoke the installed tracebox binary directly."
fi

# --- 5. Build the MCP server. ----------------------------------------------
step 5 "Building the tracebox-mcp server..."
go build -o "$MCP_BINARY" ./cmd/tracebox-mcp
MCP_PATH="$SCRIPT_DIR/$MCP_BINARY"
ok "Built MCP server: $MCP_PATH"

# --- 6. Register the MCP server with Claude Code (if available). -----------
step 6 "Registering the MCP server with Claude Code..."
# Register at *user* scope so the server is available from every directory.
# `claude mcp add` defaults to `local` scope, which only registers the server
# for the current working directory; the server would then be missing whenever
# Claude Code is launched from anywhere else. Detection and registration must
# use the same scope, so the check below uses `claude mcp get` (which resolves
# user-scoped servers from any directory) rather than parsing `claude mcp list`
# (whose local-scoped entries depend on the current directory).
MCP_ADD_CMD="claude mcp add tracebox --scope user --env TRACEBOX_API_URL=$API_URL -- \"$MCP_PATH\""
MCP_STATUS="failed"
if ! command -v claude >/dev/null 2>&1; then
    MCP_STATUS="skipped"
    warn "Claude Code not found - skipping MCP registration."
    info "Install Claude Code and re-run this script to enable MCP, or register manually:"
    info "    $MCP_ADD_CMD"
else
    # `claude mcp get tracebox` exits 0 only when a server by that name exists;
    # this avoids substring/format false positives from scraping list output.
    if claude mcp get tracebox >/dev/null 2>&1; then
        MCP_STATUS="registered"
        ok "MCP server 'tracebox' is already registered - skipping."
    elif claude mcp add tracebox --scope user --env "TRACEBOX_API_URL=$API_URL" -- "$MCP_PATH"; then
        MCP_STATUS="registered"
        ok "Registered MCP server 'tracebox' with Claude Code."
    else
        warn "Failed to register the MCP server automatically. Register manually:"
        info "    $MCP_ADD_CMD"
    fi
fi

# --- 7. Record the repo location for `tracebox start`/`tracebox stop`. ------
# The CLI binary lives on the PATH and can be invoked from anywhere, but it has
# no built-in knowledge of where this repo (and its docker-compose.yml) lives.
# Record the absolute repo path in a small config file so `tracebox start` and
# `tracebox stop` can locate the compose project from any directory.
step 7 "Recording the repo location for tracebox start/stop..."
CONFIG_DIR="$HOME/.tracebox"
CONFIG_FILE="$CONFIG_DIR/config"
mkdir -p "$CONFIG_DIR"
printf 'repo_path=%s\n' "$SCRIPT_DIR" > "$CONFIG_FILE"
ok "Recorded repo path in $CONFIG_FILE"

# --- 8. Summary. -----------------------------------------------------------
printf '\n========================================\n'
printf '%s Tracebox setup complete%s\n' "$C_GREEN" "$C_OFF"
printf '========================================\n'
printf '  Sandbox API : running at %s\n' "$API_URL"
printf '  CLI         : installed at %s\n' "$CLI_DEST"
case "$MCP_STATUS" in
    registered) printf '  MCP server  : registered with Claude Code (%s)\n' "$MCP_PATH" ;;
    skipped)    printf '  MCP server  : built but NOT registered (Claude Code not found)\n' ;;
    *)          printf '  MCP server  : built; automatic registration failed (see above)\n' ;;
esac
printf '\nYou can now run scripts in the sandbox from any directory:\n'
printf '    tracebox run script.py\n'
printf '\nManage the sandbox from any directory (no need to re-run this script):\n'
printf '    tracebox start            start the sandbox (nsjail, default)\n'
printf '    tracebox start --strict   start with the gVisor backend (stronger isolation)\n'
printf '    tracebox stop             stop the sandbox\n'
if [ "$PATH_HINT" -eq 1 ]; then
    warn "Restart your terminal (or update PATH as shown above) so 'tracebox' is found."
fi
