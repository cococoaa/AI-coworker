# 报销审核判定插件

把一家公司的报销制度变成可执行的判定。**只判定，不执行**：不付款、不改凭证、不冲销发票。

```
policy/     制度数据（财务改）—— 改阈值不用重新编译
judge/      判定逻辑（研发改）—— 零外部依赖，只 import 标准库
fixtures/   带标准答案的用例
tools/      校验工具
```

## 三步 fork

### 1. 只换制度（最常见，90% 的情况）

```bash
cp -r plugins/reimburse-audit my-audit
vim my-audit/policy/policy.json     # 改公司抬头、阈值、禁止项目、城市分档
go run ./my-audit/tools/policy-check -policy my-audit/policy/policy.json
```

`policy-check` 会告诉你：JSON 结构对不对、每条规则依赖的字段/工具是否存在、有没有判定项缺少用例。

**换制度会真的改变结论**——这是可验证的：

```bash
# 默认口径：60 条全部符合标准答案
go run ./plugins/reimburse-audit/tools/conformance

# 改成"严控版"（工作餐 100→60、招待审批线 2000→1000、住宿限额下调）
go run ./plugins/reimburse-audit/tools/conformance \
  -policy plugins/reimburse-audit/policy/presets/strict/policy.json
# → 36/60 用例结论发生变化，代码一行没改
```

内附三套 preset 作参考：`default`（默认）、`strict`（严控）、`municipal-soe`（市政国企口径）。

### 2. 加一条新规则（制度里有新条款）

要动四处，缺一处会被校验工具抓出来：

| 改哪里 | 改什么 | 漏了会怎样 |
| --- | --- | --- |
| `policy/policy.json` → `rules[]` | 加规则、加 check（id / when / outcome / requires） | `policy-check` 报"代码有、规则表没有" |
| `judge/checks_*.go` | 加判定代码（按 R01–R05 / R06–R10 / R11–R15 分段） | `policy-check` 报"规则表有、代码没实现" |
| `fixtures/cases/` | 加用例，`truth.json` 的 `hits` 写预期命中项 | `policy-check` 报"该判定项没有用例覆盖" |
| — | 跑 `conformance` | 改坏了会直接报用例编号与差异 |

### 3. 接自己的核验工具

在宿主里实现三个接口，其余不用动：

```go
type InvoiceVerifier interface {
    Verify(ctx, invoiceNo string) (judge.Verdict, []judge.InvoiceRecord)
}
type CompanyVerifier interface {
    Check(ctx, taxID string) (judge.Verdict, []judge.CompanyRecord)
}
type HistoryFinder interface {
    Find(ctx, q judge.HistoryQuery) (judge.Verdict, []judge.HistoryRecord)
}
```

## 四态语义（最容易搞错的地方）

```
Found     查到     可据此判定
NotFound  查无     可据此判定 —— "查无此票"本身就是驳回依据
Failed    失败     不可据此判定，转人工
Timeout   超时     不可据此判定，转人工 —— 既不是查到，也不是查无
```

把 Timeout 当 NotFound，系统就会拿"税务平台查无此票"去驳回一张其实正常的发票。
判定代码只能通过 `Status.Usable()` 判断能否使用，避免各处自己写字符串比较。

**这条规则有踩坑记录**：把工具状态从字符串改成四态时，R01 分支曾写成"只在 Found 时进入"，
导致"查无此票"被漏判成通过。`conformance` 当场报出 C22，才没让这个 bug 进仓库。

## 输入契约

插件只认这些类型，不认模型、不认协议：

- `Claim`：报销单的判定相关字段（申请人职级、类别、金额、日期、人数……）
- `Fields`：8 个抽取字段（发票号、开票日期、价税合计、双方税号、双方名称、发票项目）
  - 金额一律"元、两位小数"字符串；日期一律 `YYYY-MM-DD`
  - `Conf` 是合并后的置信度（两侧模型取较小值）
- `Untrusted`：两模型看到不同值、因而完全不可信的字段
- `Tools`：三个核验结果（四态 + 记录）

宿主负责把自己的数据翻译成这些类型。翻译是逐字段显式的，字段增删会编译报错，不会悄悄丢数据。

## 校验工具

| 命令 | 检查什么 | 用途 |
| --- | --- | --- |
| `go test ./plugins/reimburse-audit/...` | 一致性、依据完整性、四态语义、规则全覆盖 | CI |
| `tools/policy-check` | JSON 合法、依赖引用存在、判定项有用例覆盖 | 业务改完制度就跑 |
| `tools/conformance` | 判定实现 vs 标准答案，逐条对账 | 研发改完逻辑就跑 |

## 边界

**做**：读制度、判结论、列命中项与依据、标注"未核验/未判定"。

**不做**：付款、改凭证、冲销发票、替人做最终决定、在无法核验时放行。

**不依赖**：任何模型厂商、任何工具协议、任何数据库。插件零外部依赖，只 import 标准库。

## 目录

```
reimburse-audit/
├── plugin.json              清单：版本、层、依赖、校验命令
├── policy/
│   ├── policy.json          制度数据（唯一来源）
│   ├── rules.go             加载与结构校验
│   ├── helpers.go           参数查询（限额、席别、城市档次等）
│   ├── taxid.go             统一社会信用代码校验
│   └── presets/             三套口径示例
├── judge/
│   ├── doc.go               加规则要动哪几处
│   ├── contracts.go         四态、三个接口、记录类型
│   ├── input.go             判定输入/输出
│   ├── model.go             抽取结果的最小视图与合并逻辑
│   ├── engine.go            判定主体（按规则号分段在 checks_*.go，见下）
│   ├── support.go           佐证凭证的确定性解析
│   └── conformance_test.go  一致性测试
├── fixtures/                60 条用例 + 工具后端数据
└── tools/
    ├── policy-check/
    └── conformance/
```
