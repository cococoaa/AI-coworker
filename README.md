# 报销审核 AI 同事

## 配置密钥并启动（Windows PowerShell）

先安装 Git 和 Go 1.25 或更高版本，并确保可以联网下载 Go 依赖、访问模型接口。在 PowerShell 中执行下面的命令。

```powershell
git clone https://github.com/cococoaa/AI-coworker.git
cd AI-coworker\reimburse-agent

$env:DEEPSEEK_API_KEY = "替换为自己的网关密钥"
$env:ANTHROPIC_API_KEY = "替换为自己的网关密钥"

go run ./cmd/serve -addr 127.0.0.1:8790 -config config.example.json -data data -mcp-data data/world.json -people people.json -local-operator E3001 -local-demo-operators
```

已有代码时，直接进入克隆目录中的 `reimburse-agent` 文件夹，再设置环境变量并执行启动命令。首次启动会下载依赖和编译，请等待终端显示监听地址。

启动后，在运行服务的电脑上打开 [http://127.0.0.1:8790](http://127.0.0.1:8790)。

**密钥必须与接口地址匹配。** 当前 `config.example.json` 使用第三方网关 `https://api.openai-next.com`。如果同一把网关密钥支持两个模型，可以给两个变量填写相同的值。使用 DeepSeek、Anthropic 官方密钥或其他网关时，先调整配置文件中对应的 `base_url`、`model` 等参数。

环境变量仅对当前 PowerShell 窗口及其启动的程序生效。修改密钥后，在运行旧服务的窗口按 `Ctrl+C`，再从设置好密钥的窗口启动。密钥通过环境变量提供。

| 启动参数 | 用途 |
| --- | --- |
| `-addr 127.0.0.1:8790` | 本机访问地址和端口 |
| `-config config.example.json` | 模型接口和环境变量名称配置 |
| `-data data` | 加载仓库中的测试数据 |
| `-mcp-data data/world.json` | 使用模拟发票、企业和历史记录核验库 |
| `-people people.json` | 加载人员和权限名单 |
| `-local-operator E3001` | 将本机操作身份绑定为王会计，支持人工复核；申请人工号在表单中另填 |

需要在本机演示中切换复核人、并为留空的申请单号自动生成演示编号时，可在命令末尾追加 `-local-demo-operators`。

这套启动方式调用真实模型，并使用模拟核验数据。新上传发票是否通过还取决于核验库记录、材料完整性和业务规则。交通报销需同时提供行程凭证。

### 本机运行与远程部署的访问令牌

本机按上面的 `-addr 127.0.0.1:8790` 启动时，系统跳过网页登录令牌校验。其他人克隆仓库后在自己的电脑上使用同样的命令，也可以直接打开页面。

系统根据服务的监听地址决定是否启用登录。监听 `0.0.0.0` 等非回环地址时，必须配置访问令牌。魔搭部署默认监听 `0.0.0.0:7860`，因此会显示“请粘贴访问令牌”的登录页。

| 配置 | 用途 |
| --- | --- |
| `DEEPSEEK_API_KEY`、`ANTHROPIC_API_KEY` | 供后端调用模型接口 |
| `REIMBURSE_UI_TOKENS` | 控制网页登录，并将访问身份绑定到人员名单中的工号 |

**魔搭配置**：在创空间的密文设置中添加 `REIMBURSE_UI_TOKENS`，值使用下面的格式，将占位内容替换为至少16位的随机令牌，并重启服务使配置生效。

```text
E3001:替换为至少16位随机令牌
```

`E3001` 对应 `people.json` 中的王会计。登录框只填写冒号后面的令牌内容。
#### 共享演示登录

打开 [AI-worker 创空间](https://modelscope.cn/studios/zkkwin/AI-worker)，在应用登录框粘贴下面的令牌。

```text
62010e40f2154004b6034023760f2614
```

该共享令牌绑定 `E3001`（王会计），具备触发审核和人工复核权限。所有使用者共用这一身份，模型调用会消耗部署者配置的 API 额度。

管理员在创空间密文 `REIMBURSE_UI_TOKENS` 中配置的完整值为

```text
E3001:62010e40f2154004b6034023760f2614
```

此登录信息由部署者提供，需与运行中的密文配置一致。令牌更换后，本节中的旧令牌会失效。

**Windows 上允许其他机器访问**：先在同一个 PowerShell 窗口配置模型密钥，再设置访问令牌并启动。

```powershell
$env:REIMBURSE_UI_TOKENS = "E3001:" + [guid]::NewGuid().ToString("N")

go run ./cmd/serve -addr 0.0.0.0:8790 -config config.example.json -data data -mcp-data data/world.json -people people.json
```

登录时使用该变量中冒号后的随机字符串。其他电脑通过服务所在电脑的实际 IP 和端口访问，网络和防火墙需要允许该端口。远程访问的身份由令牌绑定，`-local-operator` 仅对本机监听生效，`-local-demo-operators` 仅支持回环地址。


> 给一个行业，造一位 AI 同事。这是财务共享中心的那一位：看票、查票、查企业、查重复、对制度，
> 给出**通过 / 调整后通过 / 退回补材料 / 转人工复核 / 驳回**，并说清依据。

## 它解决什么

财务每天要审几百张差旅餐饮报销单：抄发票字段、查验真伪、查企业状态、查重复报销、翻制度条款。
规则明确，但极耗人力。

- 一张单平均 5–8 分钟
- 判定标准靠人记条款，同一个人不同时间可能判不一样
- 查验失败时，人往往"看着没问题就过了"
- 复核结论散落在邮件里，下次遇到同一张票还是从头查

这位 AI 同事的做法是：**它做核查和初判，人只处理真正需要判断的单据。**

## 魔搭创空间

[AI-worker 创空间](https://modelscope.cn/studios/zkkwin/AI-worker) 由 GitHub Actions 从 `main` 同步到魔搭的 `master`，并以 Docker 部署。可以在 GitHub Actions 手动运行 **Deploy to ModelScope**。

- 在 GitHub 仓库的 Actions secrets 添加 `MODELSCOPE_API_KEY`，使用有权写入创空间的魔搭访问令牌。
- 在魔搭创空间密文中添加 `REIMBURSE_UI_TOKENS`，格式为 `E3001:<至少 16 字符的随机令牌>`。也可将它同名存入 GitHub Actions secrets，首次部署会自动写入创空间。登录时只填写冒号后的随机令牌。
- 默认运行合成演示，可审核内置样例。需要真实模型及 PDF 送审时，在创空间设置变量 `REIMBURSE_MODE=live`，并通过密文配置 `ANTHROPIC_API_KEY` 和 `DEEPSEEK_API_KEY`。

## 一次审核要经过什么

```
报销单 + 发票原文 + 佐证凭证
  │
  ├─ 1. 确定范围：按规则表推导本类别适用几条规则、要调哪几个工具
  ├─ 2. 双模型抽取：DeepSeek 与 Claude 各抽一遍，逐字段比对
  │      不一致 → 该字段作废；置信度 < 0.8 → 标红；只剩一个模型 → 无交叉校验，转人工
  ├─ 3. MCP 工具核验：发票查验 / 企业核验 / 历史查重，失败或超时 → 相关判定项不作判定
  ├─ 4. 规则判定：15 条规则 / 26 个判定项，命中项全部列出，结论取最严格者
  └─ 5. 审核意见：默认由规则结果拼接；让模型润色时，生成后回查金额与条款编号
```

判定优先级：**驳回 > 转人工复核 > 退回补材料 > 调整后通过 > 通过**。

## 实测结果

| 项目 | 数值 |
| --- | --- |
| 真实双模型 60 条：结论与标准答案一致 | **60 / 60** |
| 核准金额一致 | 19 / 19 |
| 命中项集合完全一致 | 59 / 60 |
| 判定项覆盖率 | 26 / 26 |
| 平均单条耗时 | 8.5 秒 |
| 平均单条成本 | $0.0101 |

唯一一条差异（C24）：两个模型都正确读出"没有购买方税号"，但一侧给这个空值只打了 0.5 置信度，
合并后判为不可信，于是 R02-b 不作判定、改由 R15-b 转人工。结论仍是驳回。
**这是交叉校验在正常工作**，详见 `reimburse-agent/docs/eval-real.md`。

## 关键设计

**幻觉与越权**
- 模型不参与判定，只抄字段和润色文字
- 润色后的意见会机器核对：出现判定结果里没有的金额或未命中的条款编号，直接丢弃
- 字段读不出、两模型不一致、置信度不足，一律"不作判定"并转人工
- 三个 MCP 工具全部只读

**过程可观测**
- 五阶段固定流水线，每步产生带序号的事件，单页实时推送（SSE）
- 工具调用能看到原始返回；每条命中项都写明"为什么"
- 跳过的判定项也列出原因，人工复核知道哪些没查

**容错**
| 故障 | 行为 |
| --- | --- |
| 一个模型超时 | 另一模型继续，关键字段无交叉校验 → 转人工 |
| 两个模型都不可用 | 不调工具、不判定 → 转人工 |
| 查验工具超时 | 视为状态未知，不当"查无此票" → 转人工 |
| 工具返回乱码 | 判为失败，不解析成空结果 |
| 发票字段污损 | 该字段作废，依赖它的判定项跳过 |

**权限与数据安全**
- 服务默认只听本机；跨机访问强制 Bearer 令牌，没配则拒绝启动
- 令牌只从环境变量或受限权限文件读，不进命令行、不进日志；日志只记令牌名与指纹
- 审计日志记调用方、来源 IP、状态码，不记业务内容
- 模型密钥只从环境变量读；配置文件出现密钥字段直接拒绝启动

## 常用命令

```
cd reimburse-agent

# 单张单据，命令行输出全过程
go run ./cmd/audit -case data/cases/C01
go run ./cmd/audit -case data/cases/C01 -fault verify_invoice=timeout

# 独立 MCP 服务
go run ./cmd/verify-mcp -data data/world.json -http 127.0.0.1:8791
go run ./cmd/verify-mcp -data data/world.json -http 0.0.0.0:8791   # 跨机：必须先配令牌

# 评测与彩排
go run ./cmd/eval -out docs
go run ./cmd/rehearse -out docs
go run ./cmd/eval -real -config config.example.json -out data/eval-real

# 重新生成测试集与规则文档
go run ./cmd/mockgen
go run ./cmd/rulesdoc
```

## 目录

```
reimburse-agent/
├── cmd/
│   ├── serve/        单页界面 + 审核服务
│   ├── audit/        命令行审核
│   ├── verify-mcp/   MCP 工具服务
│   ├── eval/         评测
│   ├── rehearse/     彩排
│   ├── mockgen/      测试集生成
│   ├── rulesdoc/     规则文档生成
│   ├── record/       模型响应录制
│   ├── probe/        单供应商连通性探针
│   └── gwdiag/       网关诊断
├── internal/
│   ├── llm/          DeepSeek / Claude 接入、交叉比对、降级、回放
│   ├── audit/        规则引擎 + 五阶段编排
│   ├── mcptools/     三个 MCP 工具的服务端与客户端
│   ├── mcpauth/      令牌鉴权
│   ├── server/       HTTP/SSE 与单页界面
│   ├── schema/       数据结构和测试集读写
│   ├── mockgen/      用例构造与自检
│   └── rehearsal/    彩排套件
├── rules/            规则表（唯一来源）
├── data/cases/       60 条测试集
└── docs/             规则、核心链路、MCP、鉴权、界面、评测、彩排、演示手册
```

## 测试

```
cd reimburse-agent
go test ./...
go vet ./...
```

覆盖：规则引擎与标准答案逐项对照（60 条）、真实 HTTP 链路（本地假供应商模拟两家协议）、
MCP 协议工具发现与故障注入、令牌鉴权、双模型降级、审核意见核对、
输入鲁棒性（12 类脏输入）、SSE 事件流与页面接口。

## 关键文档

| 文件 | 内容 |
| --- | --- |
| `docs/delivery.md` | 交付清单、验收对照、已知限制 |
| `docs/onepager.md` | 一页纸方案 |
| `docs/rules.md` | 15 条规则、26 个判定项、条款原文 |
| `docs/core.md` | 模型接入、失败处理表、三层架构 |
| `docs/mcp.md` | MCP 工具定义与启动 |
| `docs/auth.md` | 令牌鉴权与跨机访问 |
| `docs/ui.md` | 界面与接口 |
| `docs/demo.md` | 演示手册（逐句台词、常见提问） |
| `docs/eval-real.md` | 真实模型评测 |
| `docs/rehearsal-real.md` | 真实模型彩排 |
| `docs/real-integration.md` | 真实联调记录与踩到的坑 |

## 边界

**能做**：抽字段、查外部事实、按制度判初判、写审核意见、标记"未核验/未判定"。
**不能做**：付款、改凭证、冲销发票、替人做最终决定、在无法核验时放行。

**未验证**：真实票据（60 条为合成票据）、官方端点（仅验证了第三方兼容网关）。
**已知限制**：界面会话与复核记录在内存，重启即失；令牌为静态，无轮换与吊销；传输层未强制 TLS。
