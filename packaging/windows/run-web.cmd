@echo off
rem Browser GUI on localhost:8787 — the browser opens automatically
set "DIR=%~dp0"
start "" "%DIR%hearth.exe" --web
