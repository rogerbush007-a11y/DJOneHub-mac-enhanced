param(
    [string]$OutputPath = (Join-Path (Resolve-Path (Join-Path $PSScriptRoot "..")) "dist\DJOneHub.exe"),
    [ValidateSet("amd64", "arm64")]
    [string]$Arch = "amd64"
)

$ErrorActionPreference = "Stop"
$RootDir = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$OutDir = Split-Path -Parent $OutputPath
if (-not (Test-Path $OutDir)) {
    New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
}

$PrevCgo = $env:CGO_ENABLED
$PrevOs = $env:GOOS
$PrevArch = $env:GOARCH

try {
    $env:CGO_ENABLED = "0"
    $env:GOOS = "windows"
    $env:GOARCH = $Arch

    Push-Location $RootDir
    & go build -trimpath -buildvcs=false -ldflags="-s -w -H=windowsgui" -o $OutputPath ./cmd/djonehub-macos
    if ($LASTEXITCODE -ne 0) {
        throw "go build failed with exit code $LASTEXITCODE"
    }
}
finally {
    Pop-Location -ErrorAction SilentlyContinue
    $env:CGO_ENABLED = $PrevCgo
    $env:GOOS = $PrevOs
    $env:GOARCH = $PrevArch
}

# Verify PE subsystem is IMAGE_SUBSYSTEM_WINDOWS_GUI (2) so no console window pops up.
$bytes = [System.IO.File]::ReadAllBytes($OutputPath)
$peOffset = [System.BitConverter]::ToInt32($bytes, 0x3C)
$subsystem = [System.BitConverter]::ToUInt16($bytes, $peOffset + 24 + 68)
if ($subsystem -ne 2) {
    throw "Expected Windows GUI subsystem (2), got $subsystem for $OutputPath"
}

Write-Host "Built Windows GUI binary: $OutputPath (Subsystem=$subsystem, Arch=$Arch)"
