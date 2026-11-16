# MCP 服务令牌鉴权与跨机访问

核验服务默认只对本机开放。要让另一台机器上的界面或同事调用，必须开鉴权。
这一层是"权限边界"的落点：工具本身只读，鉴权决定谁能调用它。

## 1. 令牌从哪来

| 位置 | 变量 / 参数 | 说明 |
| --- | --- | --- |
| 服务端 | 环境变量 `REIMBURSE_MCP_TOKENS` | 逗号分隔，格式 `名字:令牌`；不写名字则记为 `token-N` |
| 服务端 | `-tokens-file 路径` | 每行一个，支持 `#` 注释；环境变量存在时以环境变量为准 |
| 客户端 | 环境变量 `REIMBURSE_MCP_TOKEN` | 单个令牌 |
| 客户端 | `-token-file 路径` | 取文件里第一条；环境变量优先 |

令牌一律**不放在命令行参数**里：命令行会出现在进程列表、shell 历史和监控里。
长度不足 16 位直接拒绝启动，避免现场随手设个 `123`。

生成一个：

```
powershell -c "[guid]::NewGuid().ToString('N')"
```

## 2. 启动服务

**本机演示**（不配令牌也能起，只监听 127.0.0.1）：

```
go run ./cmd/verify-mcp -data data/world.json -http 127.0.0.1:8791
```

**跨机访问**（监听所有网卡，必须先配令牌）：

```
# Windows
set REIMBURSE_MCP_TOKENS=reviewer:请换成随机串,auditor:请换成另一个随机串
verify-mcp.exe -data data\world.json -http 0.0.0.0:8791 -audit-log audit.log

# Linux / macOS
export REIMBURSE_MCP_TOKENS=reviewer:$(openssl rand -hex 24),auditor:$(openssl rand -hex 24)
./verify-mcp -data data/world.json -http 0.0.0.0:8791 -audit-log audit.log
```

Windows 上可直接用 `scripts\run-mcp-lan.cmd 8791 data\world.json`，脚本会先检查环境变量。

**没配令牌却监听 `0.0.0.0` 会直接退出**：

```
监听 0.0.0.0:8791 属于跨机访问，必须配置令牌：设置环境变量 REIMBURSE_MCP_TOKENS 或用 -tokens-file；仅本机演示可用 127.0.0.1
```

这是有意的：不允许"忘了配置就裸奔"。想强制本机也鉴权，加 `-require-auth`。

## 2.5 界面跨机访问：浏览器登录页

界面与 MCP 服务共用同一套校验，但界面多了一层对浏览器更友好的入口：

| 请求 | 行为 |
| --- | --- |
| 浏览器打开 `/` 且没有凭证 | 303 跳到 `/login`，给出只需粘贴令牌的输入框 |
| `/api/*` 没有凭证 | 仍然是 401 JSON，不会返回 HTML（脚本与监控看不到登录页） |
| `POST /login` 令牌正确 | 种 `HttpOnly; SameSite=Strict` 的 Cookie，有效期 12 小时 |
| `POST /login` 令牌错误 | 停在登录页提示"令牌不正确"，不暴露令牌名或指纹 |
| 带 `Authorization: Bearer` 的请求 | 照常放行，不受 Cookie 影响 |

早先版本直接把 `no bearer token` 渲染到浏览器里——一行白底黑字，没人知道该干什么。
现在浏览器会被引到登录框，脚本仍走 Bearer，两条路径互不干扰。

Cookie 只放令牌本身，不额外签发会话：令牌轮换后旧 Cookie 立即失效，没有"登录态残留"的问题。

## 3. 客户端接入

**本项目的命令行**：

```
set REIMBURSE_MCP_TOKEN=reviewer用的那个令牌
go run ./cmd/audit -case data/cases/C01 -mcp-url http://192.168.1.20:8791/mcp
```

令牌不对时不会给一句含糊的报错：

```
连接 MCP 工具: 连接 http://192.168.1.20:8791/mcp 被拒绝（401）：令牌缺失或不匹配。请设置环境变量 REIMBURSE_MCP_TOKEN，或用 -token-file 指定令牌文件
```

**其他 MCP 客户端**（Claude 客户端、IDE 插件等）用 HTTP 头：

```json
{
  "mcpServers": {
    "reimburse-verify": {
      "type": "http",
      "url": "http://192.168.1.20:8791/mcp",
      "headers": { "Authorization": "Bearer reviewer用的那个令牌" }
    }
  }
}
```

注意：`headers` 里写的是明文令牌。这类配置文件要放进用户目录并限制权限，
不要提交进仓库；更稳妥的做法是让客户端从环境变量插值（各客户端写法不同）。

**用 curl 自测**：

```
# 无令牌：401
curl -i -X POST http://192.168.1.20:8791/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c","version":"0"}}}'

# 有令牌：200，返回 event: message + initialize 结果
curl -i -X POST http://192.168.1.20:8791/mcp \
  -H 'Authorization: Bearer 你的令牌' \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c","version":"0"}}}'
```

`GET /healthz` 不需要令牌，只回"服务活着、是否开鉴权、三个工具名"，不含任何业务数据，
便于运维探活。

## 4. 设计上的几处取舍

- **常量时间比较**：对每个已登记令牌都做一次比较，全部比完才给结果，不提前返回，
  避免按响应时间逐字猜令牌。令牌在内存里只存 SHA-256 哈希。
- **错误信息只给指纹**：401 返回 `invalid token: 指纹 3f9fe7fd`（sha256 前 8 位）。
  运维能对上号，日志里也不会出现完整令牌。
- **一个名字 = 一个调用方**：令牌名就是审计日志里的 `client`。
  给不同的人/系统发不同令牌，出事时能追到是谁调用的。
- **审计只记元数据**：时间、调用方、来源 IP、方法、状态码、耗时。
  不记令牌，也不记请求体——请求体里有发票号、税号，日志不必留。
  401 也会被记录，用来发现"有人拿错令牌在试"。
- **经代理时取真实来源**：优先读 `X-Forwarded-For` 的第一段。

## 5. 实测记录（2026-11-16）

服务端监听 `0.0.0.0:8805`，用本机局域网地址 `172.20.188.82` 访问，模拟跨机：

| 请求 | 结果 |
| --- | --- |
| 无 Authorization | 401 `no bearer token` |
| 错误令牌 | 401 `invalid token: 指纹 86865642` |
| `reviewer` 令牌 | 200，返回完整 initialize 结果 |
| `auditor` 令牌 | 200，可完成整张单据审核（退出码 0） |
| 客户端用错误令牌 | 退出码 2，提示设置环境变量或 `-token-file` |
| 未配令牌监听 `0.0.0.0` | 启动即退出，退出码 1 |
| 令牌长度 3 位 | 启动即退出，提示至少 16 位 |

审计日志实际输出（节选）：

```json
{"at":"2026-11-16T17:00:29+08:00","client":"auditor","ip":"172.20.188.82","method":"POST","status":200,"ms":0}
{"at":"2026-11-16T17:00:29+08:00","client":"auditor","ip":"172.20.188.82","method":"GET","status":200,"ms":2}
{"at":"2026-11-16T17:00:29+08:00","client":"auditor","ip":"172.20.188.82","method":"DELETE","status":204,"ms":0}
{"at":"2026-11-16T17:00:29+08:00","ip":"172.20.188.82","method":"POST","status":401,"ms":0}
```

最后一行是错误令牌：被拒，没记到 client（因为压根没通过鉴权），但 IP 与 401 留下了。

## 6. 还没做的

1. **没有令牌轮换与吊销接口**。改令牌要重启服务（环境变量/文件重新加载）。
   演示够用，生产要有签发、过期与吊销。
2. **没有接入企业 SSO / OAuth**。当前是静态令牌，`auth.TokenInfo` 里的 `UserID` 就是令牌名，
   接 OIDC 后可以换成真实工号。
3. **传输层没有强制 TLS**。跨机时令牌是明文 Bearer 头。生产必须走 HTTPS，
   可用反向代理（Nginx/Caddy）终结 TLS，或直接用内网专线 + mTLS。
   当前 `http://` 只适合内网演示。
4. **限流只做了单次请求大小上限**（1 MB 头 + 4 MB 体），没有按令牌的速率限制。
