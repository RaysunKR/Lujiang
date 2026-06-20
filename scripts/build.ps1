# Lujiang cross-platform build matrix (PowerShell).
# Usage:
#   scripts\build.ps1                       # current OS+arch
#   scripts\build.ps1 -Os linux -Arch amd64 # override
#   scripts\build.ps1 -SkipWeb              # skip web frontend
#
# Outputs: dist\lujiang-{server,client}.{os}-{arch}[.exe]
# Requires: Go 1.25+, Node 20+ (for web assets).
#
# modernc.org/sqlite is pure-Go, so CGO_ENABLED=0 gives static binaries.

param(
    [ValidateSet("windows", "linux", "darwin")]
    [string]$Os = "",

    [ValidateSet("amd64", "arm64")]
    [string]$Arch = "",

    [switch]$SkipWeb
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot

if (-not $Os) {
    $Os = if ($IsWindows -or $env:OS -eq "Windows_NT") { "windows" }
          elseif ($IsMacOS) { "darwin" }
          else { "linux" }
}
if (-not $Arch) {
    $Arch = if ([Environment]::Is64BitOperatingSystem) { "amd64" } else { "arm64" }
}

Write-Host "==> Target: $Os/$Arch" -ForegroundColor Cyan

if (-not $SkipWeb) {
    Write-Host "==> Building web frontend" -ForegroundColor Cyan
    Push-Location web
    try {
        npm install --no-audit --no-fund
        npm run build
    } finally { Pop-Location }
}

$ext = if ($Os -eq "windows") { ".exe" } else { "" }
$dist = Join-Path $repoRoot "dist"
New-Item -ItemType Directory -Force -Path $dist | Out-Null

$env:CGO_ENABLED = "0"
$env:GOOS = $Os
$env:GOARCH = $Arch

$targets = @("lujiang-server", "lujiang-client")
foreach ($t in $targets) {
    $out = Join-Path $dist "$t.$Os-$Arch$ext"
    Write-Host "==> Building $t -> $out" -ForegroundColor Cyan
    & go build -trimpath -ldflags "-s -w" -o $out "./cmd/$t"
    if ($LASTEXITCODE -ne 0) { throw "build $t failed" }
}

Write-Host "==> Done. Artifacts in $dist" -ForegroundColor Green
