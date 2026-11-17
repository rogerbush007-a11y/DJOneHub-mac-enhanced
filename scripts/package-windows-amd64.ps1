param(
    [string]$Version = "v1.2.10"
)

$ErrorActionPreference = "Stop"
$RootDir = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$PackageName = "DJOneHub-Windows-amd64-$Version"
$StageRoot = Join-Path $RootDir "dist\release"
$StageDir = Join-Path $StageRoot $PackageName
$Archive = Join-Path $StageRoot "$PackageName.zip"
$Checksum = "$Archive.sha256"

if (Test-Path $StageDir) {
    Remove-Item -Recurse -Force $StageDir
}
New-Item -ItemType Directory -Force -Path $StageDir | Out-Null

& (Join-Path $PSScriptRoot "build-windows.ps1") -OutputPath (Join-Path $StageDir "DJOneHub.exe") -Arch "amd64"

Copy-Item -Force (Join-Path $RootDir "windows\install.ps1") (Join-Path $StageDir "install.ps1")
Copy-Item -Force (Join-Path $RootDir "windows\uninstall.ps1") (Join-Path $StageDir "uninstall.ps1")
Copy-Item -Force (Join-Path $RootDir "windows\Install DJOneHub.cmd") (Join-Path $StageDir "Install DJOneHub.cmd")
Copy-Item -Force (Join-Path $RootDir "windows\Stop DJOneHub.cmd") (Join-Path $StageDir "Stop DJOneHub.cmd")
Copy-Item -Force (Join-Path $RootDir "windows\README-Windows.txt") (Join-Path $StageDir "README-Windows.txt")
Copy-Item -Force (Join-Path $RootDir "LICENSE") (Join-Path $StageDir "LICENSE")
Copy-Item -Force (Join-Path $RootDir "THIRD_PARTY_NOTICES.md") (Join-Path $StageDir "THIRD_PARTY_NOTICES.md")

if (Test-Path $Archive) {
    Remove-Item -Force $Archive
}
if (Test-Path $Checksum) {
    Remove-Item -Force $Checksum
}

Compress-Archive -Path $StageDir -DestinationPath $Archive -CompressionLevel Optimal
$hash = (Get-FileHash -Path $Archive -Algorithm SHA256).Hash.ToLowerInvariant()
"$hash  $(Split-Path -Leaf $Archive)" | Set-Content -Path $Checksum -Encoding ascii

Write-Host "Release directory: $StageDir"
Write-Host "Release archive:   $Archive"
Write-Host "Checksum:          $Checksum"
