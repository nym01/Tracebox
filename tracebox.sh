#!/usr/bin/env sh
# tracebox.sh — one-command setup for Tracebox (Linux / macOS).
#
# Works two ways, auto-detected:
#
#   REPO mode       — run from a git clone (docker-compose.yml + go.mod sit next
#                     to this script). Builds the sandbox image and the CLI/MCP
#                     binaries from source. Requires Docker and Go. This is the
#                     contributor/developer path and is unchanged.
#
#   STANDALONE mode — run on its own with no repo and no Go (e.g. fetched via
#                     `curl -fsSL .../tracebox.sh | sh`). Pulls the prebuilt
#                     sandbox image from ghcr.io and downloads prebuilt CLI/MCP
#                     binaries from the GitHub release. Requires only Docker.
#
# Either way it: starts the sandbox API and waits until healthy, installs the
# CLI onto your PATH, sets up the MCP server, records where the compose project
# lives so `tracebox start`/`tracebox stop` work anywhere, and prints a summary.
#
# Safe to re-run.

set -eu

# --- Distribution coordinates (STANDALONE mode). ---------------------------
IMAGE="ghcr.io/nym01/tracebox:latest"
RELEASE_BASE="https://github.com/nym01/Tracebox/releases/download/latest"
TRACEBOX_DIR="$HOME/.tracebox"
STANDALONE_COMPOSE="$TRACEBOX_DIR/docker-compose.yml"

# --- Locate the directory containing this script (if any). -----------------
# When piped (`curl ... | sh`) there is no script file; fall back to the CWD.
if [ -n "${0:-}" ] && [ -f "$0" ]; then
    SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
else
    SCRIPT_DIR=$(pwd)
fi
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

# download URL DEST — fetch URL to DEST using whichever client is available.
download() {
    if command -v curl >/dev/null 2>&1; then
        curl -fSL --retry 3 -o "$2" "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$2" "$1"
    else
        fail "Neither curl nor wget is available to download $1"
    fi
}

# --- Detect REPO vs STANDALONE mode. ---------------------------------------
# REPO mode needs both the compose file and the Go module next to the script;
# anything else is a standalone install.
if [ -f "$SCRIPT_DIR/docker-compose.yml" ] && [ -f "$SCRIPT_DIR/go.mod" ]; then
    MODE=repo
else
    MODE=standalone
fi

printf 'Tracebox setup (%s mode)\n' "$MODE"
[ "$MODE" = repo ] && printf 'Repo: %s\n' "$SCRIPT_DIR"

# Resolve the OS/arch target name used by the release assets (standalone only).
TARGET=""
if [ "$MODE" = standalone ]; then
    os=$(uname -s); arch=$(uname -m)
    case "$os" in
        Linux)  os=linux ;;
        Darwin) os=darwin ;;
        *) fail "Unsupported OS for standalone install: $os (use the git-clone path instead)." ;;
    esac
    case "$arch" in
        x86_64|amd64)  arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        *) fail "Unsupported architecture for standalone install: $arch (use the git-clone path instead)." ;;
    esac
    TARGET="${os}-${arch}"
fi

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

# --- 2. Go (REPO mode only). -----------------------------------------------
step 2 "Checking Go..."
if [ "$MODE" = repo ]; then
    if ! command -v go >/dev/null 2>&1; then
        fail "Go not found - install from https://go.dev, then re-run this script.
       (Or use the standalone install, which needs no Go: see the README.)"
    fi
    ok "Go is installed ($(go version))."
else
    ok "Standalone mode - skipping Go (prebuilt binaries will be downloaded)."
fi

# --- 3. Start the sandbox API and wait for it to be healthy. ---------------
if [ "$MODE" = repo ]; then
    step 3 "Starting the sandbox API (docker compose up -d --build)..."
    info "First build compiles nsjail and can take a few minutes - please wait."
    if ! docker compose up -d --build; then
        fail "docker compose failed to start the sandbox API. See the output above."
    fi
else
    step 3 "Starting the sandbox API (prebuilt image)..."
    mkdir -p "$TRACEBOX_DIR"
    # Minimal compose referencing the published image. Mirrors the repo
    # docker-compose.yml but uses image: instead of build:. GOBOXD_RUNNER stays
    # parameterised so `tracebox start --strict` selects the gVisor backend.
    cat > "$STANDALONE_COMPOSE" <<EOF
services:
  goboxd:
    image: $IMAGE
    ports:
      - "8080:8080"
    environment:
      GOBOXD_RUNNER: \${GOBOXD_RUNNER:-nsjail}
      TRACEBOX_DB_PATH: /data/tracebox.db
    volumes:
      - tracebox-data:/data
    privileged: true
    cgroup: host

volumes:
  tracebox-data:
EOF
    ok "Wrote $STANDALONE_COMPOSE"
    info "First start pulls the sandbox image and can take a few minutes - please wait."
    if ! docker compose -f "$STANDALONE_COMPOSE" up -d; then
        fail "docker compose failed to start the sandbox API. See the output above."
    fi
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

# --- 4. Acquire and install the CLI. ---------------------------------------
step 4 "Installing the tracebox CLI..."
if [ "$MODE" = repo ]; then
    go build -o "$CLI_BINARY" ./cmd/tracebox-cli
    info "Built ./$CLI_BINARY"
    SRC="$SCRIPT_DIR/$CLI_BINARY"
else
    DL_DIR=$(mktemp -d)
    SRC="$DL_DIR/$CLI_BINARY"
    info "Downloading prebuilt CLI ($TARGET)..."
    download "$RELEASE_BASE/tracebox-cli-$TARGET" "$SRC"
    chmod +x "$SRC"
fi

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
    info "or add this folder to your PATH (in ~/.bashrc or ~/.zshrc):"
    info "    export PATH=\"$(dirname "$SRC"):\$PATH\""
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

# --- 5. Acquire the MCP server. --------------------------------------------
step 5 "Installing the tracebox-mcp server..."
if [ "$MODE" = repo ]; then
    go build -o "$MCP_BINARY" ./cmd/tracebox-mcp
    MCP_PATH="$SCRIPT_DIR/$MCP_BINARY"
else
    # Install the prebuilt MCP server next to the CLI so it has a stable absolute
    # path for `claude mcp add`.
    MCP_PATH="$INSTALL_DIR/$MCP_BINARY"
    info "Downloading prebuilt MCP server ($TARGET)..."
    download "$RELEASE_BASE/tracebox-mcp-$TARGET" "$MCP_PATH"
    chmod +x "$MCP_PATH"
fi
ok "MCP server: $MCP_PATH"

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

# --- 7. Record the compose location for `tracebox start`/`tracebox stop`. ---
# The CLI binary lives on the PATH and can be invoked from anywhere, but it has
# no built-in knowledge of where the compose project lives. Record it so
# `tracebox start` / `tracebox stop` can locate the project from any directory:
#   repo_path     in repo mode      (build from the clone)
#   compose_file  in standalone mode (run the prebuilt-image compose)
step 7 "Recording the sandbox location for tracebox start/stop..."
CONFIG_DIR="$HOME/.tracebox"
CONFIG_FILE="$CONFIG_DIR/config"
mkdir -p "$CONFIG_DIR"
if [ "$MODE" = repo ]; then
    printf 'repo_path=%s\n' "$SCRIPT_DIR" > "$CONFIG_FILE"
    ok "Recorded repo path in $CONFIG_FILE"
else
    printf 'compose_file=%s\n' "$STANDALONE_COMPOSE" > "$CONFIG_FILE"
    ok "Recorded compose file in $CONFIG_FILE"
fi

# --- 8. Summary. -----------------------------------------------------------
printf '\n========================================\n'
printf '%s Tracebox setup complete (%s mode)%s\n' "$C_GREEN" "$MODE" "$C_OFF"
printf '========================================\n'
printf '  Sandbox API : running at %s\n' "$API_URL"
printf '  CLI         : installed at %s\n' "$CLI_DEST"
case "$MCP_STATUS" in
    registered) printf '  MCP server  : registered with Claude Code (%s)\n' "$MCP_PATH" ;;
    skipped)    printf '  MCP server  : installed but NOT registered (Claude Code not found)\n' ;;
    *)          printf '  MCP server  : installed; automatic registration failed (see above)\n' ;;
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
