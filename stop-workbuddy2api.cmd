@echo off
setlocal
cd /d "%~dp0"

set "WB2API_EXE=%CD%\wb2api.exe"
set "WB2API_PID_FILE=%CD%\wb2api.pid"
if not exist "%WB2API_PID_FILE%" (
  echo WorkBuddy2API is not running: no PID file.
  exit /b 0
)

set /p WB2API_PID=<"%WB2API_PID_FILE%"
powershell -NoProfile -Command "try { $p=Get-Process -Id ([int]$env:WB2API_PID) -ErrorAction Stop; if ([IO.Path]::GetFullPath($p.Path) -eq [IO.Path]::GetFullPath($env:WB2API_EXE)) { exit 0 } } catch {}; exit 1"
if errorlevel 1 (
  echo Removed stale PID file; no process was stopped.
  del /q "%WB2API_PID_FILE%" >nul 2>&1
  exit /b 0
)

taskkill /PID %WB2API_PID% /T /F >nul 2>&1
if errorlevel 1 (
  echo Failed to stop WorkBuddy2API PID=%WB2API_PID%.
  exit /b 1
)
del /q "%WB2API_PID_FILE%" >nul 2>&1
echo WorkBuddy2API stopped.
endlocal
