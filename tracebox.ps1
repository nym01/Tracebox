# tracebox.ps1 — one-command setup for Tracebox (Windows / PowerShell).
#
# Takes a fresh clone to a fully working setup:
#   1. Checks Docker is installed and running.
#   2. Checks Go is installed.
#   3. Starts the sandbox API with docker compose and waits until it is healthy.
#   4. Builds and installs the tracebox CLI onto your user PATH.
#   5. Builds the tracebox-mcp MCP server.
#   6. Registers the MCP server with Claude Code (if the `claude` CLI is found).
#   7. Prints a summary of what was set up.
#
# Safe to re-run: existing containers, an already-installed CLI and an
# already-registered MCP server are detected and left alone / updated.

$ErrorActionPreference = "Stop"

# --- Locate the repo root (the directory containing this script). ----------
$RepoRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $RepoRoot

$ApiUrl    = "http://localhost:8080"
$CliBinary = "tracebox.exe"
$McpBinary = "tracebox-mcp.exe"

function Write-Step($n, $msg) { Write-Host "`n[$n/7] $msg" -ForegroundColor Cyan }
function Write-Ok($msg)       { Write-Host "  OK  $msg"  -ForegroundColor Green }
function Write-Info($msg)     { Write-Host "      $msg" }
function Write-Warn2($msg)    { Write-Host "  !!  $msg"  -ForegroundColor Yellow }
function Fail($msg) {
    Write-Host "`nERROR: $msg" -ForegroundColor Red
    exit 1
}

Write-Host "Tracebox setup" -ForegroundColor White
Write-Host "Repo: $RepoRoot"

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

# --- 2. Go installed. ------------------------------------------------------
Write-Step 2 "Checking Go..."
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Fail @"
Go not found - install from https://go.dev, then re-run this script.
(Pre-built binaries may be offered as an alternative in a future release.)
"@
}
Write-Ok "Go is installed ($((go version) 2>$null))."

# --- 3. Start the sandbox API and wait for it to be healthy. ---------------
Write-Step 3 "Starting the sandbox API (docker compose up -d --build)..."
Write-Info "First build compiles nsjail and can take a few minutes - please wait."
docker compose up -d --build
if ($LASTEXITCODE -ne 0) {
    Fail "docker compose failed to start the sandbox API. See the output above."
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

# --- 4. Build and install the CLI. -----------------------------------------
Write-Step 4 "Building and installing the tracebox CLI..."
go build -o $CliBinary ./cmd/tracebox-cli
if ($LASTEXITCODE -ne 0) { Fail "Failed to build the CLI." }
Write-Info "Built .\$CliBinary"

$InstallDir = Join-Path $env:USERPROFILE "tracebox\bin"
if (-not (Test-Path $InstallDir)) {
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
}
$CliDest = Join-Path $InstallDir $CliBinary
Copy-Item -Path (Join-Path $RepoRoot $CliBinary) -Destination $CliDest -Force
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

# --- 5. Build the MCP server. ----------------------------------------------
Write-Step 5 "Building the tracebox-mcp server..."
go build -o $McpBinary ./cmd/tracebox-mcp
if ($LASTEXITCODE -ne 0) { Fail "Failed to build the MCP server." }
$McpPath = (Resolve-Path (Join-Path $RepoRoot $McpBinary)).Path
Write-Ok "Built MCP server: $McpPath"

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

# --- 7. Summary. -----------------------------------------------------------
Write-Host "`n========================================" -ForegroundColor White
Write-Host " Tracebox setup complete" -ForegroundColor Green
Write-Host "========================================" -ForegroundColor White
Write-Host "  Sandbox API : running at $ApiUrl"
Write-Host "  CLI         : installed at $CliDest"
if ($McpRegistered)   { Write-Host "  MCP server  : registered with Claude Code ($McpPath)" }
elseif ($McpSkipped)  { Write-Host "  MCP server  : built but NOT registered (Claude Code not found)" }
else                  { Write-Host "  MCP server  : built; automatic registration failed (see above)" }
Write-Host ""
Write-Host "You can now run scripts in the sandbox from any directory:"
Write-Host "    tracebox run script.py"
if ($PathChanged) {
    Write-Warn2 "Open a NEW terminal first so the updated PATH takes effect."
}
