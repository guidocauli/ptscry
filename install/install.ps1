# ptscry-gui installer (Windows) - Guido Cauli <guido.cauli@gmail.com>
#
#   install.bat                         install for the current user (no administrator needed)
#   install.bat -System                 install for every user (asks for administrator rights)
#   install.bat -Uninstall [-System]    remove
#   install.bat -AddToPath              also put the folder in PATH (so "ptscry-gui" works from a prompt)
#
# User:   %LOCALAPPDATA%\Programs\ptscry-gui, Start menu of the user
# System: %ProgramFiles%\ptscry-gui,          Start menu of everybody
# The program appears in "Apps and features" (Settings) and can be removed from there.
# The settings in ptscry profile are never touched.

param(
    [switch]$System,
    [switch]$Uninstall,
    [switch]$AddToPath,
    [string]$Dir = ""
)
$ErrorActionPreference = "Stop"
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$name = "ptscry-gui"

function Test-Admin {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    (New-Object Security.Principal.WindowsPrincipal $id).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

if ($System -and -not (Test-Admin)) {
    # relaunch elevated with the same switches
    $argl = @("-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "`"$($MyInvocation.MyCommand.Path)`", "-System")
    if ($Uninstall) { $argl += "-Uninstall" }
    if ($AddToPath) { $argl += "-AddToPath" }
    if ($Dir) { $argl += @("-Dir", "`"$Dir`"") }
    Start-Process powershell -Verb RunAs -ArgumentList $argl -Wait
    exit
}

if ($System) {
    $target = if ($Dir) { $Dir } else { Join-Path $env:ProgramFiles $name }
    $menu = Join-Path $env:ProgramData "Microsoft\Windows\Start Menu\Programs"
    $reg = "HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall\$name"
    $pathScope = "Machine"
} else {
    $target = if ($Dir) { $Dir } else { Join-Path $env:LOCALAPPDATA "Programs\$name" }
    $menu = Join-Path $env:APPDATA "Microsoft\Windows\Start Menu\Programs"
    $reg = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\$name"
    $pathScope = "User"
}
$link = Join-Path $menu "$name.lnk"

function Remove-FromPath($dir, $scope) {
    $p = [Environment]::GetEnvironmentVariable("Path", $scope)
    if (-not $p) { return }
    $parts = $p.Split(";") | Where-Object { $_ -and ($_.TrimEnd("\") -ine $dir.TrimEnd("\")) }
    [Environment]::SetEnvironmentVariable("Path", ($parts -join ";"), $scope)
}

if ($Uninstall) {
    Write-Host "==> removing $target"
    Get-Process -Name $name -ErrorAction SilentlyContinue | Where-Object { $_.Path -like "$target\*" } | Stop-Process -Force
    Remove-Item -Force $link -ErrorAction SilentlyContinue
    Remove-Item -Recurse -Force $reg -ErrorAction SilentlyContinue
    Remove-FromPath $target $pathScope
    # the uninstaller itself is in $target: remove the rest after this script ends
    Remove-Item -Recurse -Force $target -ErrorAction SilentlyContinue
    Write-Host "==> done (the settings of ptscry-gui were left alone)"
    exit
}

$files = @("ptscry-gui.exe", "ptscry.exe")
foreach ($f in $files) {
    if (-not (Test-Path (Join-Path $here $f))) { throw "$f not found next to this script (unpack the whole zip first)" }
}
Write-Host "==> installing in $target"
New-Item -ItemType Directory -Force $target | Out-Null
Get-Process -Name $name -ErrorAction SilentlyContinue | Where-Object { $_.Path -like "$target\*" } | Stop-Process -Force
foreach ($f in ($files + @("ptscry-gui.ico", "install.ps1", "install.bat", "GUI.md", "README.md"))) {
    $src = Join-Path $here $f
    if (Test-Path $src) { Copy-Item -Force $src $target }
}

# Start menu shortcut with the icon
New-Item -ItemType Directory -Force $menu | Out-Null
$sh = New-Object -ComObject WScript.Shell
$lnk = $sh.CreateShortcut($link)
$lnk.TargetPath = Join-Path $target "ptscry-gui.exe"
$lnk.WorkingDirectory = $env:USERPROFILE
$lnk.IconLocation = (Join-Path $target "ptscry-gui.exe") + ",0"
$lnk.Description = "ptscry-gui - Guido Cauli <guido.cauli@gmail.com>"
$lnk.Save()

# "Apps and features" entry
$ver = "0.7.8-gui"
try { $v = (Get-Item (Join-Path $target "ptscry-gui.exe")).VersionInfo.ProductVersion; if ($v) { $ver = $v } } catch {}
New-Item -Force $reg | Out-Null
$uninst = "powershell.exe -NoProfile -ExecutionPolicy Bypass -File `"$target\install.ps1`" -Uninstall" + $(if ($System) { " -System" } else { "" })
Set-ItemProperty $reg DisplayName "ptscry-gui"
Set-ItemProperty $reg DisplayVersion $ver
Set-ItemProperty $reg Publisher "Guido Cauli <guido.cauli@gmail.com>"
Set-ItemProperty $reg DisplayIcon ((Join-Path $target "ptscry-gui.exe") + ",0")
Set-ItemProperty $reg InstallLocation $target
Set-ItemProperty $reg UninstallString $uninst
Set-ItemProperty $reg NoModify 1 -Type DWord
Set-ItemProperty $reg NoRepair 1 -Type DWord

if ($AddToPath) {
    $p = [Environment]::GetEnvironmentVariable("Path", $pathScope)
    if (-not (($p -split ";") -contains $target)) {
        [Environment]::SetEnvironmentVariable("Path", ($p.TrimEnd(";") + ";" + $target), $pathScope)
        Write-Host "==> $target added to PATH (open a new prompt)"
    }
}
Write-Host "==> done: ptscry-gui is in the Start menu"
