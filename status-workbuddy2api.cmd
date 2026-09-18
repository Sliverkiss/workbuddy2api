@echo off
setlocal
cd /d "%~dp0"

set "WB2API_EXE=%CD%\wb2api.exe"
set "WB2API_PID_FILE=%CD%\wb2api.pid"
if not exist "%WB2API_PID_FILE%" (
  echo WorkBuddy2API is not running: no PID file.
  exit /b 1
)

set /p WB2API_PID=<"%WB2API_PID_FILE%"
powershell -NoProfile -Command "try { $p=Get-Process -Id ([int]$env:WB2API_PID) -ErrorAction Stop; if ([IO.Path]::GetFullPath($p.Path) -eq [IO.Path]::GetFullPath($env:WB2API_EXE)) { exit 0 } } catch {}; exit 1"
if errorlevel 1 (
  echo WorkBuddy2API is not running: stale PID file.
  exit /b 1
)

echo WorkBuddy2API is running. PID=%WB2API_PID%
if "%WB2API_HEALTH_URL%"=="" set "WB2API_HEALTH_URL=http://127.0.0.1:7863/healthz"
curl.exe --fail-with-body --silent --show-error --max-time 5 "%WB2API_HEALTH_URL%"
set "WB2API_STATUS=%ERRORLEVEL%"
echo.
exit /b %WB2API_STATUS%
