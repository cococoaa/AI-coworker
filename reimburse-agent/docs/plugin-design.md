# 插件化设计：把规则表与判定引擎做成可 fork 的包

> 目标：把 `rules/rules.json` 与 `internal/audit` 从"这个项目里的一段代码"变成
> "别人 fork 回去、替换自己的制度就能用"的营业单元。
>
> 参照 `anthropics/financial-services` 的组织方式：**一切以文件为准，按工种分层，接口与实现分离**。
> 但有一处根本差异：它的判定靠模型遵循 prompt，本插件把判定写成编译期就固定下来的 Go 代码，
> 规则表只是数据。所以"换规则"分两级——换阈值改 JSON，换判定逻辑改代码 + 跑一致性测试。

---

## 一、分层依据

三层，按"谁会改它"划分，而不是按技术模块划分。这是插件能不能被 fork 的关键：
运维、业务、研发各自只碰一层，不互相牵动。

| 层 | 谁改 | 改什么 | 是否需要重新编译 | 是否需要重新测 |
| --- | --- | --- | --- | --- |
| **Policy**（制度数据） | 财务、内控 | 阈值、条款原文、禁止项目、城市分档、公司抬头 | 否（支持外部文件） | 只需跑 `policy-check` |
| **Judge**（判定逻辑） | 研发 | 新增/修改判定项的代码 | 是 | 必须跑 `conformance` + 全量评测 |
| **Harness**（运行外壳） | 平台/集成 | 接哪个模型、哪个 MCP 服务、SSE、鉴权 | 否 | 冒烟测试 |

对应到目录，就是 `policy/`、`judge/`、`harness/`（harness 保持现在的 `internal/server`、`cmd/`）。
插件边界只包前两层，第三层留给你自己的宿主程序——这样插件可以嵌进任何 Go 服务。

---

## 二、目录设计

```
reimburse-agent/
│
├── plugins/reimburse-audit/          ← 插件根：可整体 fork 的最小单元
│   ├── plugin.json                   ← 清单（仿 Claude 插件 marketplace.json）
│   ├── README.md                     ← fork 三步走 + 契约说明
│   ├── VERSION                       ← 语义版本，与 policy 版本分别演进
│   │
│   ├── policy/                       ← 第一层：制度数据，业务自己改
│   │   ├── policy.json               ← 即现在的 rules.json（顶层键不变）
│   │   ├── clause-index.md           ← 由条款生成，给人读的对照表
│   │   └── presets/
│   │       ├── default/              ← 当前这套（星河科技，虚构）
│   │       │   └── policy.json
│   │       ├── strict/               ← 更严：金额容差 0、无缓冲期、招待线减半
│   │       │   └── policy.json
│   │       └── municipal-soe/        ← 市政国企口径示例（差旅标准更紧）
│   │           └── policy.json
│   │
│   ├── judge/                        ← 第二层：判定逻辑，研发改
│   │   ├── judge.go                  ← 引擎入口（现在 engine.go 的公开面）
│   │   ├── checks_r01_r05.go         ← 按规则段拆文件，一条规则一个文件更好 review
│   │   ├── checks_r06_r10.go
│   │   ├── checks_r11_r15.go
│   │   ├── support.go                ← 佐证凭证的确定性解析
│   │   ├── contracts.go              ← 输入/输出契约（不依赖任何上层）
│   │   ├── registry.go               ← 判定项注册表：编号 ↔ 实现 ↔ 依赖的字段/工具
│   │   ├── conformance_test.go       ← 用 policy 里的用例跑自检
│   │   └── doc.go                    ← 包注释：写清"加一条规则要动哪几处"
│   │
│   ├── fixtures/                     ← 第三层资产：测试与演示共用
│   │   ├── cases/                    ← 60 条用例
│   │   └── world.json
│   │
│   └── tools/                        ← 给 fork 者的辅助命令（不进宿主二进制）
│       ├── policy-check/             ← 只校验 policy：结构、引用完整性、覆盖
│       └── conformance/              ← 判定实现 vs 标准答案，逐项对账
│
├── internal/                          ← 宿主侧，不属于插件
│   ├── llm/                           ← 模型接入（与插件无关）
│   ├── mcptools/  mcpauth/            ← 工具层（插件只依赖接口）
│   └── server/                        ← 界面
└── cmd/                               ← 可执行入口
```

### 为什么 `judge/` 不放在 `internal/` 下

`internal/` 在 Go 里意味着"外部模块不可导入"。插件要能被别人 fork 后当**普通包**引入
（或者替换后仍能编译），就必须放在 `internal/` 之外。这是本次改造中唯一无法回避的
结构变更，代价是所有 import 路径要改一遍。

---

## 三、插件契约（谁依赖谁）

**插件不依赖宿主的任何包。** 依赖方向单向：

```
host (cmd/, internal/server, internal/llm, internal/mcptools)
        │  依赖
        ▼
plugins/reimburse-audit/judge  ──依赖──▶  policy/*.json（数据）
        │                                  
        └─ 只依赖：标准库 + plugin 内的 contracts.go
```

具体三个接口，都由插件定义、宿主实现：

| 接口 | 方法 | 宿主实现 | 说明 |
| --- | --- | --- | --- |
| `judge.InvoiceVerifier` | `Verify(ctx, invoiceNo) Verdict` | 包 MCP `verify_invoice` | 返回「查到/查无/失败/超时」四态 |
| `judge.CompanyVerifier` | `Check(ctx, taxID) Verdict` | 包 MCP `check_company` | 同上 |
| `judge.HistoryFinder` | `Find(ctx, query) Records` | 包 MCP `check_history` | 返回相关记录，不整库 |

三态语义是契约的核心，必须在 `contracts.go` 里写死并加测试：

```
Verdict{Status: "found", ...}   → 可以据此判定
Verdict{Status: "not_found"}    → 可以据此判定（查无此票=驳回依据）
Verdict{Status: "failed"}       → 不可据此判定，转人工
Verdict{Status: "timeout"}      → 不可据此判定，转人工（状态未知，既非通过也非查无）
```

**这是整个插件最容易被 fork 者搞错的地方**：把 timeout 当成 not_found，系统就会拿"查无此票"去驳回。
所以契约里要把它做成类型而不是字符串枚举，让写错就编译不过。

### 字段契约

```
Field{Name, Value, Confidence, Source}
  Name   : 8 个固定字段名（枚举，不是字符串）
  Value  : 金额一律"元，两位小数"字符串；日期一律 YYYY-MM-DD
  Confidence: 0..1，由交叉比对后取较小值
```

`judge` 只认 `Field`，不知道字段是哪个模型抽的、用的什么协议。这样换模型不影响插件。

---

## 四、规则表的三处改造

现在的 `rules.json` 是"规则 + 参数 + 条款"混在一份文件里。要 fork，得让**参数**和**条款**分开流动：

| 现状 | 改造后 | 原因 |
| --- | --- | --- |
| `params` 与 `rules` 同文件 | 同文件，但 `params` 单独成节并有 `$schema` 引用 | 业务只改 `params`，不动 `rules` |
| 条款文本 `clause_text` 内嵌 | 保留，另生成 `clause-index.md` | 合规要能整篇审阅，不想读 JSON |
| 每条规则 `basis` 只写类别 | 增加 `basis_ref` 字段，指向制度文件的具体条目 | 现在只能写"内控通行做法"，fork 者要能填自己的制度号 |
| 判定项 `outcome` 是字符串 | 不变，但加 `requires` 字段声明依赖的字段与工具 | 让"哪些不可信就跳过"变成数据，不是代码里散落的硬编码 |

`requires` 是最关键的改动。现在字段/工具依赖写在 `params.field_deps` 和 `params.tool_deps` 里，
集中但不直观。改成每条 check 自己声明：

```json
{"id": "R10-a", "when": "...", "outcome": "approve_with_adjustment",
 "requires": {"fields": ["total_amount"], "tools": [], "support": ["nights"]}}
```

这样新增一条规则时，依赖是**跟规则写在一起**的，不会忘。原来的 `field_deps` 保留生成（由 `requires` 反向汇总），
用于校验"有没有规则引用了不存在的字段"。

---

## 五、改动清单

### A. 目录与包路径（必做）

| # | 动作 | 涉及文件 | 风险 |
| --- | --- | --- | --- |
| A1 | 新建 `plugins/reimburse-audit/judge/`，把 `internal/audit` 的非测试源码迁入 | `engine.go`、`support.go`、`pipeline.go`、`spy.go`、`mcp_adapter.go` | 中：import 路径全变 |
| A2 | `engine.go` 拆成 `judge.go` + `checks_r01_r05.go` / `checks_r06_r10.go` / `checks_r11_r15.go` | 同上 | 低：纯移动，逐段搬迁后跑测试 |
| A3 | `pipeline.go` 留在插件内（它是"五阶段流水线"，是插件的编排能力，不是宿主的） | `pipeline.go` | 低 |
| A4 | `mcp_adapter.go` 移除，改由宿主在 `internal/server` 与 `cmd/` 里实现 `Judge` 的三个接口 | `mcp_adapter.go`、`internal/server/tools.go`、`cmd/audit/tools.go` | 中：接口要重新对齐 |
| A5 | 全局替换 import：`reimburse/internal/audit` → `reimburse/plugins/reimburse-audit/judge` | cmd/*、internal/server/*、internal/rehearsal/*、测试 | 低但量大 |

### B. 契约落地（必做）

| # | 动作 | 产出 |
| --- | --- | --- |
| B1 | 新建 `judge/contracts.go`：`Field`、`FieldName` 枚举、`Verdict` 类型与四态常量、三个接口 | 一个文件，约 150 行 |
| B2 | `Verdict` 用类型而非字符串：`type Status uint8`，四态常量；`func (s Status) Usable() bool` | 让"误用 timeout"编译不过 |
| B3 | 现有 `ToolResult` 改为宿主侧类型，插件内只出现 `Verdict` | 消除插件对 `schema` 包以外的依赖 |

### C. 规则表改造（必做）

| # | 动作 | 产出 |
| --- | --- | --- |
| C1 | `policy.json` 增加顶层 `$schema` 与 `plugin_version` | 便于校验版本匹配 |
| C2 | 每条 check 增加 `requires` 字段 | 26 个判定项各补一行 |
| C3 | `Load()` 兼容旧格式（无 `requires` 时从 `field_deps`/`tool_deps` 反推） | 不破坏现有用例 |
| C4 | 新增 `presets/strict`、`presets/municipal-soe` 两套参数 | 证明"换制度只换 JSON" |
| C5 | 生成 `clause-index.md`（现有 `cmd/rulesdoc` 改造） | 合规可整篇审阅 |

### D. 校验工具（必做）

| # | 命令 | 检查什么 | 退出码 |
| --- | --- | --- | --- |
| D1 | `tools/policy-check` | JSON 结构、`requires` 引用存在、每个判定项至少一条用例覆盖、`outcome` 合法 | 0/1 |
| D2 | `tools/conformance` | 判定实现 vs 标准答案，逐项对账（现在 `TestEngineMatchesLabelOnAllCases` 的命令行版） | 0/1 |
| D3 | 二者都支持 `-policy` 参数指定任意 preset | fork 者改完自己跑 | — |

### E. 文档（必做）

| # | 文件 | 内容 |
| --- | --- | --- |
| E1 | `plugin.json` | 名称、版本、entrypoint 包、依赖的 policy 版本、三个接口名 |
| E2 | `README.md` | fork 三步：改 policy → 跑 policy-check → 跑 conformance |
| E3 | `judge/doc.go` | "加一条规则要动哪几处"的清单 |
| E4 | `policy/README.md` | 每个 `params` 字段的含义与取值建议 |

### F. 不动的部分（明确边界）

- `internal/llm`：模型接入与插件无关，插件只吃 `Field`
- `internal/mcptools`、`internal/mcpauth`：工具层，插件只依赖接口
- `internal/server`、`cmd/*`：宿主
- `rules/taxid.go`：统一社会信用代码校验是通用能力，移到 `judge/taxid.go` 或留在 `rules/` 作为公共包

---

## 六、fork 者的三步

```bash
# 1. 只换制度（最常见）
cp -r plugins/reimburse-audit my-audit
vim my-audit/policy/policy.json        # 改公司抬头、阈值、禁止项目
go run ./tools/policy-check -policy my-audit/policy/policy.json

# 2. 改判定逻辑（制度里有新条款）
vim my-audit/judge/checks_r11_r15.go   # 加一个判定项
vim my-audit/policy/policy.json        # 同步加进 rules[]
go run ./tools/conformance -policy my-audit/policy/policy.json -cases my-audit/fixtures/cases

# 3. 接自己的工具
# 在宿主里实现 judge.InvoiceVerifier / CompanyVerifier / HistoryFinder 三个方法
```

---

## 七、验收标准

改造完成后必须满足：

1. `go test ./...` 全绿，60 条用例结论、命中项、核准金额与改造前完全一致
2. `policy-check` 对三套 preset 都通过
3. `conformance` 对 `default` preset 输出 60/60
4. 把 `policy.json` 的某个阈值改掉，`conformance` 能报出受影响的用例编号（证明规则真的从数据来）
5. 插件包不 import 任何 `reimburse/internal/*`
6. 宿主去掉插件后仍能 `go build`（证明边界干净）

---

## 八、这次不做的事

- **不做动态加载**（Go plugin 包跨平台限制多）。fork 是源码级 fork，不是运行时装载。
- **不做多插件并存**。一个进程一套规则，换规则靠换 preset。
- **不重写判定逻辑**。这次只搬家 + 补契约 + 拆文件，判定行为一个字不改——所以才有第 1 条验收标准。
