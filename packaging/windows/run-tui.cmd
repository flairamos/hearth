@echo off
rem Launch hearth (interactive TUI) in a terminal window (Windows Terminal if available)
set "DIR=%~dp0"
where wt >nul 2>nul
if %errorlevel%==0 (
  start "" wt -d "%DIR%" "%DIR%hearth.exe"
) else (
  start "" cmd /k "%DIR%hearth.exe"
)
