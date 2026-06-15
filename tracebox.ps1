# tracebox.ps1 — one-command setup for Tracebox (Windows / PowerShell).
#
# Works two ways, auto-detected:
#
#   REPO mode       — run from a git clone (docker-compose.yml + go.mod sit next
#                     to this script). Builds the sandbox image and the CLI/MCP
#                     binaries from source. Requires Docker and Go. This is the
#                     contributor/developer path and is unchanged.
#
#   STANDALONE mode — run on its own with no repo and no Go (e.g. fetched via
#                     `irm .../tracebox.ps1 | iex`). Pulls the prebuilt sandbox
#                     image from ghcr.io and downloads prebuilt CLI/MCP binaries
#                     from the GitHub release. Requires only Docker.
#
# Either way it: starts the sandbox API and waits until healthy, installs the
# CLI onto your user PATH, sets up the MCP server, records where the compose
# project lives so `tracebox start`/`tracebox stop` work anywhere, and prints a
# summary.
#
# Safe to re-run.

$ErrorActionPreference = "Stop"

# --- Distribution coordinates (STANDALONE mode). ---------------------------
$Image             = "ghcr.io/nym01/tracebox:latest"
$ReleaseBase       = "https://github.com/nym01/Tracebox/releases/download/latest"
$TraceboxDir       = Join-Path $env:USERPROFILE ".tracebox"
$StandaloneCompose = Join-Path $TraceboxDir "docker-compose.yml"

# --- Locate the directory containing this script (if any). -----------------
# When piped (`irm ... | iex`) there is no script file; fall back to the CWD.
$ScriptPath = $MyInvocation.MyCommand.Path
if ($ScriptPath) {
    $RepoRoot = Split-Path -Parent $ScriptPath
    Set-Location $RepoRoot
} else {
    $RepoRoot = (Get-Location).Path
}

$ApiUrl    = "http://localhost:8080"
$CliBinary = "tracebox.exe"
$McpBinary = "tracebox-mcp.exe"

function Write-Step($n, $msg) { Write-Host "`n[$n/8] $msg" -ForegroundColor Cyan }
function Write-Ok($msg)       { Write-Host "  OK  $msg"  -ForegroundColor Green }
function Write-Info($msg)     { Write-Host "      $msg" }
function Write-Warn2($msg)    { Write-Host "  !!  $msg"  -ForegroundColor Yellow }
function Fail($msg) {
    Write-Host "`nERROR: $msg" -ForegroundColor Red
    exit 1
}

# --- Detect REPO vs STANDALONE mode. ---------------------------------------
# REPO mode needs both the compose file and the Go module next to the script;
# anything else is a standalone install.
if ((Test-Path (Join-Path $RepoRoot "docker-compose.yml")) -and
    (Test-Path (Join-Path $RepoRoot "go.mod"))) {
    $Mode = "repo"
} else {
    $Mode = "standalone"
}

Write-Host "Tracebox setup ($Mode mode)" -ForegroundColor White
if ($Mode -eq "repo") { Write-Host "Repo: $RepoRoot" }

# Resolve the OS/arch target name used by the release assets (standalone only).
$Target = $null
if ($Mode -eq "standalone") {
    $arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
    $Target = "windows-$arch"
}

# --- 1. Docker installed and running. --------------------------------------
Write-Step 1 "Checking Docker..."
if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    Fail @"
Docker not found - install from https://docker.com/get-started,
make sure Docker Desktop is running, then re-run this script.
"@
}
# `docker info` only succeeds when the daemon is actually running.
docker info *> $null
if ($LASTEXITCODE -ne 0) {
    Fail @"
Docker is installed but the daemon is not responding.
Start Docker Desktop, wait for it to finish starting, then re-run this script.
"@
}
Write-Ok "Docker is installed and running."

# --- 2. Go (REPO mode only). -----------------------------------------------
Write-Step 2 "Checking Go..."
if ($Mode -eq "repo") {
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        Fail @"
Go not found - install from https://go.dev, then re-run this script.
(Or use the standalone install, which needs no Go: see the README.)
"@
    }
    Write-Ok "Go is installed ($((go version) 2>$null))."
} else {
    Write-Ok "Standalone mode - skipping Go (prebuilt binaries will be downloaded)."
}

# --- 3. Start the sandbox API and wait for it to be healthy. ---------------
if ($Mode -eq "repo") {
    Write-Step 3 "Starting the sandbox API (docker compose up -d --build)..."
    Write-Info "First build compiles nsjail and can take a few minutes - please wait."
    docker compose up -d --build
    if ($LASTEXITCODE -ne 0) {
        Fail "docker compose failed to start the sandbox API. See the output above."
    }
} else {
    Write-Step 3 "Starting the sandbox API (prebuilt image)..."
    if (-not (Test-Path $TraceboxDir)) {
        New-Item -ItemType Directory -Force -Path $TraceboxDir | Out-Null
    }
    # Minimal compose referencing the published image. Mirrors the repo
    # docker-compose.yml but uses image: instead of build:. GOBOXD_RUNNER stays
    # parameterised so `tracebox start --strict` selects the gVisor backend.
    $ComposeContent = @"
services:
  goboxd:
    image: $Image
    ports:
      - "8080:8080"
    environment:
      GOBOXD_RUNNER: `${GOBOXD_RUNNER:-nsjail}
      TRACEBOX_DB_PATH: /data/tracebox.db
    volumes:
      - tracebox-data:/data
    privileged: true
    cgroup: host

volumes:
  tracebox-data:
"@
    [System.IO.File]::WriteAllText($StandaloneCompose, $ComposeContent, (New-Object System.Text.UTF8Encoding $false))
    Write-Ok "Wrote $StandaloneCompose"
    Write-Info "First start pulls the sandbox image and can take a few minutes - please wait."
    docker compose -f $StandaloneCompose up -d
    if ($LASTEXITCODE -ne 0) {
        Fail "docker compose failed to start the sandbox API. See the output above."
    }
}
Write-Info "Containers are up. Waiting for the API to become healthy..."

$TimeoutSec = 120
$deadline   = (Get-Date).AddSeconds($TimeoutSec)
$healthOk   = $false
$readyOk    = $false

while ((Get-Date) -lt $deadline) {
    if (-not $healthOk) {
        try {
            $r = Invoke-WebRequest -Uri "$ApiUrl/healthz" -UseBasicParsing -TimeoutSec 5
            if ($r.StatusCode -eq 200) { $healthOk = $true; Write-Ok "/healthz is up." }
        } catch { }
    }
    if ($healthOk -and -not $readyOk) {
        try {
            $r = Invoke-WebRequest -Uri "$ApiUrl/readyz" -UseBasicParsing -TimeoutSec 5
            if ($r.StatusCode -eq 200) { $readyOk = $true; Write-Ok "/readyz reports ready." }
        } catch { }
    }
    if ($healthOk -and $readyOk) { break }
    Start-Sleep -Seconds 3
}

if (-not $healthOk) {
    Write-Warn2 "The API did not pass /healthz within $TimeoutSec seconds."
    Write-Info  "Check the logs with:  docker compose logs"
    Fail "Sandbox API is not healthy."
}
if (-not $readyOk) {
    Write-Warn2 "/healthz is up but /readyz is not fully ready (a language probe may be degraded)."
    Write-Info  "Continuing anyway - check 'docker compose logs' if runs fail. Details: $ApiUrl/readyz"
} else {
    Write-Ok "Sandbox API is healthy at $ApiUrl"
}

# --- 4. Acquire and install the CLI. ---------------------------------------
Write-Step 4 "Installing the tracebox CLI..."
$InstallDir = Join-Path $env:USERPROFILE "tracebox\bin"
if (-not (Test-Path $InstallDir)) {
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
}
$CliDest = Join-Path $InstallDir $CliBinary

if ($Mode -eq "repo") {
    go build -o $CliBinary ./cmd/tracebox-cli
    if ($LASTEXITCODE -ne 0) { Fail "Failed to build the CLI." }
    Write-Info "Built .\$CliBinary"
    Copy-Item -Path (Join-Path $RepoRoot $CliBinary) -Destination $CliDest -Force
} else {
    Write-Info "Downloading prebuilt CLI ($Target)..."
    Invoke-WebRequest -Uri "$ReleaseBase/tracebox-cli-$Target.exe" -OutFile $CliDest -UseBasicParsing
}
Write-Ok "Installed CLI: $CliDest"

# Add the bin folder to the *user* PATH (no admin rights required).
$UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($null -eq $UserPath) { $UserPath = "" }
$onPath = $UserPath.Split(';') | Where-Object { $_.Trim().TrimEnd('\') -ieq $InstallDir.TrimEnd('\') }
$PathChanged = $false
if (-not $onPath) {
    $newPath = if ([string]::IsNullOrEmpty($UserPath)) { $InstallDir } else { "$UserPath;$InstallDir" }
    try {
        [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
        Write-Ok "Added $InstallDir to your user PATH."
        $PathChanged = $true
    } catch {
        Write-Warn2 "Could not update your PATH automatically. Add this folder manually:"
        Write-Info  "    $InstallDir"
        Write-Info  "(Settings - Edit environment variables for your account - Path - New)"
        $PathChanged = $true
    }
} else {
    Write-Ok "$InstallDir is already on your user PATH."
}

# Confirm the binary is callable (use the freshly installed copy directly).
try {
    & $CliDest --help *> $null
    Write-Ok "tracebox is callable."
} catch {
    Write-Warn2 "Could not invoke the installed tracebox binary directly."
}

# --- 5. Acquire the MCP server. --------------------------------------------
Write-Step 5 "Installing the tracebox-mcp server..."
if ($Mode -eq "repo") {
    go build -o $McpBinary ./cmd/tracebox-mcp
    if ($LASTEXITCODE -ne 0) { Fail "Failed to build the MCP server." }
    $McpPath = (Resolve-Path (Join-Path $RepoRoot $McpBinary)).Path
} else {
    # Install the prebuilt MCP server next to the CLI so it has a stable absolute
    # path for `claude mcp add`.
    $McpPath = Join-Path $InstallDir $McpBinary
    Write-Info "Downloading prebuilt MCP server ($Target)..."
    Invoke-WebRequest -Uri "$ReleaseBase/tracebox-mcp-$Target.exe" -OutFile $McpPath -UseBasicParsing
}
Write-Ok "MCP server: $McpPath"

# --- 6. Register the MCP server with Claude Code (if available). -----------
Write-Step 6 "Registering the MCP server with Claude Code..."
# Register at *user* scope so the server is available from every directory.
# `claude mcp add` defaults to `local` scope, which only registers the server
# for the current working directory; the server would then be missing whenever
# Claude Code is launched from anywhere else. Detection and registration must
# use the same scope, so the check below uses `claude mcp get` (which resolves
# user-scoped servers from any directory) rather than parsing `claude mcp list`
# (whose local-scoped entries depend on the current directory).
$McpAddCmd = "claude mcp add tracebox --scope user --env TRACEBOX_API_URL=$ApiUrl -- `"$McpPath`""
$McpRegistered = $false
$McpSkipped    = $false
if (-not (Get-Command claude -ErrorAction SilentlyContinue)) {
    $McpSkipped = $true
    Write-Warn2 "Claude Code not found - skipping MCP registration."
    Write-Info  "Install Claude Code and re-run this script to enable MCP, or register manually:"
    Write-Info  "    $McpAddCmd"
} else {
    # `claude mcp get tracebox` exits 0 only when a server by that name exists;
    # this avoids substring/format false positives from scraping list output.
    claude mcp get tracebox *> $null
    if ($LASTEXITCODE -eq 0) {
        $McpRegistered = $true
        Write-Ok "MCP server 'tracebox' is already registered - skipping."
    } else {
        claude mcp add tracebox --scope user --env "TRACEBOX_API_URL=$ApiUrl" -- "$McpPath"
        if ($LASTEXITCODE -eq 0) {
            $McpRegistered = $true
            Write-Ok "Registered MCP server 'tracebox' with Claude Code."
        } else {
            Write-Warn2 "Failed to register the MCP server automatically. Register manually:"
            Write-Info  "    $McpAddCmd"
        }
    }
}

# --- 7. Record the compose location for `tracebox start`/`tracebox stop`. ---
# The CLI binary lives on the PATH and can be invoked from anywhere, but it has
# no built-in knowledge of where the compose project lives. Record it so
# `tracebox start` / `tracebox stop` can locate the project from any directory:
#   repo_path     in repo mode      (build from the clone)
#   compose_file  in standalone mode (run the prebuilt-image compose)
Write-Step 7 "Recording the sandbox location for tracebox start/stop..."
$ConfigDir  = Join-Path $env:USERPROFILE ".tracebox"
$ConfigFile = Join-Path $ConfigDir "config"
if (-not (Test-Path $ConfigDir)) {
    New-Item -ItemType Directory -Force -Path $ConfigDir | Out-Null
}
# Write a simple key=value config (UTF-8, no BOM).
if ($Mode -eq "repo") {
    $ConfigContent = "repo_path=$RepoRoot`n"
} else {
    $ConfigContent = "compose_file=$StandaloneCompose`n"
}
[System.IO.File]::WriteAllText($ConfigFile, $ConfigContent, (New-Object System.Text.UTF8Encoding $false))
Write-Ok "Recorded sandbox location in $ConfigFile"

# --- 8. Summary. -----------------------------------------------------------
Write-Host "`n========================================" -ForegroundColor White
Write-Host " Tracebox setup complete ($Mode mode)" -ForegroundColor Green
Write-Host "========================================" -ForegroundColor White
Write-Host "  Sandbox API : running at $ApiUrl"
Write-Host "  CLI         : installed at $CliDest"
if ($McpRegistered)   { Write-Host "  MCP server  : registered with Claude Code ($McpPath)" }
elseif ($McpSkipped)  { Write-Host "  MCP server  : installed but NOT registered (Claude Code not found)" }
else                  { Write-Host "  MCP server  : installed; automatic registration failed (see above)" }
Write-Host ""
Write-Host "You can now run scripts in the sandbox from any directory:"
Write-Host "    tracebox run script.py"
Write-Host ""
Write-Host "Manage the sandbox from any directory (no need to re-run this script):"
Write-Host "    tracebox start            start the sandbox (nsjail, default)"
Write-Host "    tracebox start --strict   start with the gVisor backend (stronger isolation)"
Write-Host "    tracebox stop             stop the sandbox"
if ($PathChanged) {
    Write-Warn2 "Open a NEW terminal first so the updated PATH takes effect."
}
