@echo off
rem Build the delivery bundle into dist\.
rem Produces: reimburse-agent.exe (web UI), audit.exe (CLI), verify-mcp.exe (MCP server),
rem plus test data, docs and example configs. No secrets are packaged.
setlocal
set "ROOT=%~dp0.."
set "OUT=%ROOT%\dist"

echo [1/4] clean
if exist "%OUT%" rd /s /q "%OUT%"
mkdir "%OUT%"
mkdir "%OUT%\data"
mkdir "%OUT%\docs"

echo [2/4] build
pushd "%ROOT%"
go build -trimpath -ldflags "-s -w" -o "%OUT%\reimburse-agent.exe" ./cmd/serve
if errorlevel 1 goto fail
go build -trimpath -ldflags "-s -w" -o "%OUT%\audit.exe" ./cmd/audit
if errorlevel 1 goto fail
go build -trimpath -ldflags "-s -w" -o "%OUT%\verify-mcp.exe" ./cmd/verify-mcp
if errorlevel 1 goto fail

echo [3/4] copy assets
xcopy /e /i /q "%ROOT%\data\cases" "%OUT%\data\cases" >nul
copy /y "%ROOT%\data\world.json" "%OUT%\data\world.json" >nul
copy /y "%ROOT%\config.example.json" "%OUT%\config.example.json" >nul
copy /y "%ROOT%\mcp.example.json" "%OUT%\mcp.example.json" >nul
copy /y "%ROOT%\README.md" "%OUT%\README.md" >nul
copy /y "%ROOT%\docs\*.md" "%OUT%\docs\" >nul
copy /y "%ROOT%\scripts\run-mcp-lan.cmd" "%OUT%\run-mcp-lan.cmd" >nul
popd

echo [4/4] smoke check
"%OUT%\reimburse-agent.exe" -h >nul 2>&1
if errorlevel 2 goto fail
echo done: %OUT%
dir /b "%OUT%"
exit /b 0

:fail
echo build failed with %errorlevel%
exit /b 1
