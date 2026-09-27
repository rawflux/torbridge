@echo off
setlocal
title TorBridge Uninstall
net session >nul 2>&1
if errorlevel 1 (
    echo Requesting administrator rights...
    powershell -NoProfile -Command "Start-Process -FilePath '%~f0' -Verb RunAs"
    exit /b
)
echo This removes the tor service, the TorBridge scheduled tasks and C:\Tor.
set /p "_=Press Enter to continue or close this window to cancel..."
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" -Uninstall
echo.
if errorlevel 1 (echo FAILED: see the messages above.) else (echo SUCCESS: TorBridge removed.)
echo.
set /p "_=Press Enter to close..."
