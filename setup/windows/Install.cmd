@echo off
setlocal
title TorBridge Setup
rem One-click install: asks for administrator rights, runs install.ps1, shows the result.
net session >nul 2>&1
if errorlevel 1 (
    echo Requesting administrator rights...
    powershell -NoProfile -Command "Start-Process -FilePath '%~f0' -Verb RunAs"
    exit /b
)
cd /d "%~dp0"
echo ================================================================
echo  TorBridge Setup: Tor client with automatically refreshed bridges
echo ================================================================
echo.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1"
set RC=%ERRORLEVEL%
echo.
echo ================================================================
if "%RC%"=="0" goto ok
if "%RC%"=="3" goto partial
echo  FAILED: installation did not complete (code %RC%).
echo  See the messages above. Log: C:\Tor\logs\torbridge.log
goto done
:ok
echo  SUCCESS: Tor is installed and connected.
echo  SOCKS proxy 127.0.0.1:9050, HTTP proxy 127.0.0.1:9080
echo  Bridges refresh weekly; force now: schtasks /run /tn \TorBridge\UpdateNow
goto done
:partial
echo  INSTALLED, but the Tor connection is not up yet.
echo  It will be retried automatically when the network is available.
:done
echo ================================================================
echo.
set /p "_=Press Enter to close..."
exit /b %RC%
