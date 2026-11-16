# MCP 工具层

三个核验工具是独立进程里的 MCP 服务，不是进程内函数。审核流程通过 MCP 协议调用它们，
所以"工具在进程外、调用可拦截、返回可替换"在演示时可以直接验证。

## 工具清单

| 工具 | 入参 | 返回 | 用到的规则 |
| --- | --- | --- | --- |
| `verify_invoice` | `invoice_no` | `status`（ok/failed/timeout）、`found`、`invoices[]`（开票日期、价税合计、购买方与销售方税号、状态） | R01-a/b/c |
| `check_company` | `tax_id` | `status`、`found`、`companies[]`（名称、城市、登记状态） | R13-a/b |
| `check_history` | `invoice_no`、`applicant_id`、`seller_tax_id`（至少一个） | `status`、`records[]`（单号、发票号、申请人、销售方、开票日期、金额、类别、状态） | R04-a/b、R05-b |

三条共同的约定：

1. **只读**。三个工具没有任何写操作，不能改凭证、改报销单或冲销发票。评审关心的"越权"从工具层就直接掐掉。
2. **只回相关记录**。按发票号、税号、申请人等最少条件查询，历史记录超过 200 条截断并标记 `truncated`，不整库返回。
3. **"查无"和"查不了"必须分开**：
   - `found=false` + `status=ok`：确定查无此票/此企业，规则据此判驳回；
   - `status=failed` / `timeout`：本次未核验，规则不作判定，转人工。

   这两者在代码里是不同的类型、不同的分支，各有测试锁死。曾有一个隐患：把超时当成"空结果"，就会被理解成"查无此票"从而误驳回。现在 `classify()` 遇到解析失败一律判 `failed`，绝不退化成空列表。

金额一律以**元字符串**返回（`"553.00"`），不是分。模型不做单位换算，就不会在换算上出错；分只用于内部计算，客户端再转回来。

## 启动方式

令牌与跨机访问见 `docs/auth.md`。监听 `0.0.0.0` 时必须配令牌，否则拒绝启动。

```
# 独立进程，HTTP 方式（推荐演示用，可用 curl 或浏览器开发者工具直接看请求）
go run ./cmd/verify-mcp -data data/world.json -http 127.0.0.1:8791

# stdio 方式（供 MCP 客户端拉起）
go run ./cmd/verify-mcp -data data/world.json

# 故障注入：整类工具或单个键
go run ./cmd/verify-mcp -data data/world.json -fault verify_invoice=timeout
go run ./cmd/verify-mcp -data data/world.json -fault check_company:913101xx=error
```

`-data` 可以指向单个 `world.json`，也可以指向目录（合并目录下所有 `world.json`，用于跨单据查重）。

健康检查与工具清单：

```
curl -s http://127.0.0.1:8791/healthz
# {"ok":true,"tools":["verify_invoice","check_company","check_history"]}
```

直接走 MCP 协议调用（先 initialize 拿 session，再 tools/list、tools/call）已经在 11 月 16 日实测通过，
返回里同时带 `content`（人读的文本）与 `structuredContent`（程序解析用）。

## 与审核流程的连接

`cmd/audit` 不再读本地文件当工具：

```
# 进程内起 MCP 服务（走完整 MCP 协议，只是传输不走网络），一条命令即可
go run ./cmd/audit -case data/cases/C01

# 连独立进程 / 远程服务（跨机需令牌，见 docs/auth.md）
go run ./cmd/audit -case data/cases/C01 -mcp-url http://127.0.0.1:8791/mcp
```

启动时会做一次工具自检：三个工具必须都能被发现，缺一个直接报错退出，不允许带着残缺的工具集去审核。

每次调用都留痕：工具名、入参、状态、耗时、原始返回文本，写进事件流与报告，界面直接展示。这一层是 `audit.RemoteSpy` 包装的，换任何实现都不会丢可观测性。

## 类型转换为什么这么绕

`internal/mcptools` 不导入 `internal/audit`：audit 的测试要导入 mcptools，互相导入会形成导入环。
所以两边各定义一份结果结构，由 `audit.WrapMCP` 做逐字段转换。字段增减会立刻编译失败，
不会出现"新增字段悄悄丢掉"的情况。

## 测试

| 测试 | 证明什么 |
| --- | --- |
| `internal/mcptools/mcp_test.go` | 真实 MCP 协议下的工具发现、入参 schema、调用与解析；三种故障注入；按 key 注入；客户端超时；乱码判 failed；参数缺失 |
| `internal/mcptools/http_test.go` | Streamable HTTP 传输下三个工具都能正常工作 |
| `internal/audit/mcp_e2e_test.go` | 60 条用例经 MCP 链路判定，结论与核准金额和标准答案完全一致；每次调用都留痕；工具故障时仍转人工 |
| `internal/audit/engine_test.go` | 规则引擎与标准答案在 60 条用例上逐项一致 |

## 当前缺口

1. 后端数据仍是本项目生成的 mock 库（`data/world.json`，59 张发票、38 家企业、70 条历史记录）。
   接口已定，换成真实税务查验与企业信息查询时只改 `DataSource` 实现。
2. ~~没有做 MCP 鉴权~~ 已补齐：令牌鉴权、审计日志与跨机配置见 `docs/auth.md`。
3. `verify_invoice` 的返回字段按本项目的 mock 库设计，真实税局查验接口的字段名与状态码需要重新映射。
