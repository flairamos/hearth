@echo off
rem Open a terminal with hearth-agent usage hints
set "DIR=%~dp0"
"%DIR%hearth-agent.exe" help
echo.
echo Examples:
echo   %DIR%hearth-agent agent "task" --yolo
echo   %DIR%hearth-agent ask "question"
echo   %DIR%hearth-agent models
echo.
cmd /k
