param([switch]$NoLaunch)

$ErrorActionPreference = "Stop"
$SourceDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$InstallDir = Join-Path $env:LOCALAPPDATA "Programs\DJOneHub"
$StartMenu = Join-Path $env:APPDATA "Microsoft\Windows\Start Menu\Programs"
$Executable = Join-Path $InstallDir "DJOneHub.exe"

Write-Host "Installing DJOneHub to $InstallDir"
Get-Process -Name "DJOneHub" -ErrorAction SilentlyContinue | Stop-Process -Force

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Copy-Item -Force (Join-Path $SourceDir "DJOneHub.exe") $Executable
Copy-Item -Force (Join-Path $SourceDir "uninstall.ps1") (Join-Path $InstallDir "uninstall.ps1")
Copy-Item -Force (Join-Path $SourceDir "README-Windows.txt") (Join-Path $InstallDir "README-Windows.txt")
Copy-Item -Force (Join-Path $SourceDir "LICENSE") (Join-Path $InstallDir "LICENSE")
Copy-Item -Force (Join-Path $SourceDir "THIRD_PARTY_NOTICES.md") (Join-Path $InstallDir "THIRD_PARTY_NOTICES.md")

$Shell = New-Object -ComObject WScript.Shell
$Shortcut = $Shell.CreateShortcut((Join-Path $StartMenu "DJOneHub.lnk"))
$Shortcut.TargetPath = $Executable
$Shortcut.WorkingDirectory = $InstallDir
$Shortcut.Description = "DJOneHub for DJI first-generation 4G module"
$Shortcut.Save()

$UninstallShortcut = $Shell.CreateShortcut((Join-Path $StartMenu "Uninstall DJOneHub.lnk"))
$UninstallShortcut.TargetPath = "powershell.exe"
$UninstallShortcut.Arguments = "-NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File `"$(Join-Path $InstallDir 'uninstall.ps1')`""
$UninstallShortcut.WorkingDirectory = $InstallDir
$UninstallShortcut.WindowStyle = 7
$UninstallShortcut.Save()

Write-Host "DJOneHub installation completed."
if (-not $NoLaunch) {
    Start-Process $Executable
}
