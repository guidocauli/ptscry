@echo off
rem ptscry-gui installer (Windows) - Guido Cauli <guido.cauli@gmail.com>
rem Double click = install for the current user. Options: see install.ps1
rem   install.bat -System | -Uninstall | -AddToPath
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" %*
if errorlevel 1 pause
