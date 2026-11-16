@echo off
rem 跨机演示启动脚本：监听所有网卡并要求令牌。
rem 令牌从环境变量 REIMBURSE_MCP_TOKENS 读取，格式 名字:令牌，逗号分隔。
rem 没有配置令牌时服务会拒绝启动 —— 这是有意的，不允许"忘了配就裸奔"。

if "%REIMBURSE_MCP_TOKENS%"=="" (
  echo [错误] 未设置 REIMBURSE_MCP_TOKENS。
  echo 请先设置，例如：
  echo    set REIMBURSE_MCP_TOKENS=reviewer:请换成随机串,auditor:请换成随机串
  echo 生成随机串：powershell -c "[guid]::NewGuid().ToString('N')"
  exit /b 1
)

set PORT=%1
if "%PORT%"=="" set PORT=8791
set DATA=%2
if "%DATA%"=="" set DATA=data\world.json

echo 启动 MCP 核验服务：0.0.0.0:%PORT% ，数据 %DATA%
echo 令牌只从环境变量读取，不会出现在命令行里。
verify-mcp.exe -data "%DATA%" -http 0.0.0.0:%PORT% -audit-log audit.log
