# install.ps1 — build the Tracebox CLI and put it on your PATH (Windows).
#
# Builds cmd/tracebox-cli into tracebox.exe and installs it to
# %USERPROFILE%\tracebox\bin, then adds that folder to your *user* PATH (no
# admin rights required). After this you can run `tracebox run script.py` from
# any directory.
#
# This installs only the CLI client. The sandbox API server still needs to be
# running separately (see the repo README: `docker compose up -d --build`).

$ErrorActionPreference = "Stop"

# --- Locate the repo root (the directory containing this script). ----------
$RepoRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $RepoRoot

$Binary = "tracebox.exe"

# --- Check prerequisites. --------------------------------------------------
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Error "Go is not installed (or not on your PATH). Install Go from https://go.dev/dl/ and try again."
    exit 1
}

# --- Build. ----------------------------------------------------------------
Write-Host "Building $Binary from $RepoRoot ..."
go build -o $Binary ./cmd/tracebox-cli
if ($LASTEXITCODE -ne 0) {
    Write-Error "Build failed."
    exit 1
}
Write-Host "Built .\$Binary"

# --- Install to a user-owned bin folder. -----------------------------------
$InstallDir = Join-Path $env:USERPROFILE "tracebox\bin"
if (-not (Test-Path $InstallDir)) {
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
}
$Dest = Join-Path $InstallDir $Binary
Copy-Item -Path (Join-Path $RepoRoot $Binary) -Destination $Dest -Force
Write-Host ""
Write-Host "Installed: $Dest"

# --- Add the bin folder to the user PATH (not System / not admin). ---------
$UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($null -eq $UserPath) { $UserPath = "" }

$onPath = $UserPath.Split(';') | Where-Object { $_.Trim().TrimEnd('\') -ieq $InstallDir.TrimEnd('\') }

if (-not $onPath) {
    $newPath = if ([string]::IsNullOrEmpty($UserPath)) { $InstallDir } else { "$UserPath;$InstallDir" }
    try {
        [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
        Write-Host "Added $InstallDir to your user PATH."
        $PathChanged = $true
    } catch {
        Write-Host ""
        Write-Host "Could not update your PATH automatically. Add this folder to your PATH manually:"
        Write-Host "    $InstallDir"
        Write-Host "(Settings - Edit environment variables for your account - Path - New)"
        $PathChanged = $true
    }
} else {
    Write-Host "$InstallDir is already on your user PATH."
    $PathChanged = $false
}

# --- Success. --------------------------------------------------------------
Write-Host ""
Write-Host "Done! You can now run Tracebox from any directory:"
Write-Host "    tracebox run script.py"
if ($PathChanged) {
    Write-Host "(open a new terminal first so the updated PATH takes effect)"
}
