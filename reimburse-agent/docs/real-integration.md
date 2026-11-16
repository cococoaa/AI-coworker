# 真实双模型联调记录

> 2026-11-16，首次用真实密钥跑通 DeepSeek + Claude 整条链路，并把结果固化为可复跑的报告。

## 一、接入的到底是什么

两个模型走同一个第三方网关 `https://api.openai-next.com`，**协议不同**：

| 侧 | 协议 | 端点 | 模型名 | 结构化输出方式 |
| --- | --- | --- | --- | --- |
| DeepSeek | OpenAI 兼容 | `/v1/chat/completions` | `deepseek-v3.2` | `response_format={"type":"json_object"}` |
| Claude | Anthropic 兼容 | `/v1/messages` | `claude-sonnet-5-5` | `output_config.format`（JSON Schema） |

两个环境变量填同一个密钥：

```
DEEPSEEK_API_KEY=<网关密钥>
ANTHROPIC_API_KEY=<网关密钥>
```

配置见 `config.example.json`。模型名、端点、UA 都是配置项，换网关只改这份文件。

## 二、踩到的四个坑（都已修）

**1. 网关拦非浏览器 UA**
直接请求返回 `403 error code: 1010`（Cloudflare 的 UA 拦截）。加上浏览器 UA 立刻 200。
→ 配置里新增 `user_agent`，客户端请求头带上。

**2. `deepseek-chat` 在这个网关上 404**
网关的模型列表里 463 个模型，DeepSeek 系列有 `deepseek-v3.2`、`deepseek-v4-flash`、`deepseek-r1` 等，
但 `deepseek-chat` 返回 `bad_response_status_code`。改用 `deepseek-v3.2`。

**3. token 用量一直显示 0**
`decodeUsage` 用 `map[string]json.Number` 反序列化 usage。这个网关的 usage 里有
`service_tier:"standard"`、`output_tokens_details:{...}` 这类非数字字段，
导致整个反序列化失败，用量全成 0——成本永远是 $0.00000。
→ 改成 `map[string]any` 逐个键取数字，遇到非数字跳过。加了 `TestDecodeUsageVariants` 覆盖四种形态。
**这个 bug 只在真实网关上暴露，本地假供应商测不出来。**

**4. 配置文件里 `api_key_env` 被误判成"写了密钥"**
原来的检查是"全文转小写找 `api_key\"`"，而字段名 `api_key_env` 就以它开头，
于是任何配置都拒绝启动。→ 改成解析成结构后递归检查键名，只认完全等于
`api_key`/`key`/`secret`/`token` 等的键。顺带允许 `_` 开头的说明键。

## 三、跑出来的结果

### 单张单据（C01，命令行）

```
Claude   抽取完成 —— 输入 2050 / 输出 322 token，耗时 2971ms
DeepSeek 抽取完成 —— 输入 670 / 输出 153 token，耗时 11539ms（并行）
8 个字段合并后置信度均为 1.00，两模型完全一致
结论：通过，核准 ¥553.00，总耗时 10.3s，成本 $0.00770
```

### 60 条全量评测（`docs/eval-real.md`）

| 指标 | 数值 |
| --- | --- |
| 结论与标准答案一致 | **60 / 60（100%）** |
| 核准金额一致 | **19 / 19** |
| 命中项集合完全一致 | 59 / 60 |
| 判定项精确率 / 召回率 | 0.981 / 0.981 |
| 判定项覆盖 | 26 / 26 |
| 平均单条耗时 | 8.5 秒 |
| 平均单条成本 | $0.0101 |

唯一差异是 C24（个人抬头且无购买方税号）：两个模型都正确读出"没有税号"，
但 DeepSeek 给这个空值只打了 0.5 置信度，合并后该字段判为不可信，
于是 R02-b 不作判定、改由 R15-b 转人工。结论仍是驳回，只是依据少了一条。
**这是交叉校验在正常工作**：一方对"空"没把握时，系统不替它拍板。

### 4 场彩排（`docs/rehearsal-real.md`）

| 场景 | 结论 | 耗时 |
| --- | --- | --- |
| 工具故障：发票查验超时 | 转人工 | 6.9s |
| 模型超时：Claude 不可用 | 转人工 | 5.8s |
| 单模型不可用：关闭 DeepSeek | 转人工 | 6.6s |
| 对照：一切正常 | 通过 | 5.7s |

彩排里还修了一个 bug：耗时统计用 `defer` 改局部变量，而返回值是值类型，
导致记录里全是 0ms。改用命名返回值。

## 四、成本与性能的实话

- **平均 8.5 秒/单**：两家模型并行调用（Claude 3–7s，DeepSeek 5–12s）+ 三次工具查询。
  比合成模式慢得多，现场演示要预期这个节奏。
- **平均 $0.0101/单**：Claude 占约 90%。DeepSeek 单次只要 $0.0004 左右。
  若要压成本，把 Claude 换成网关上的便宜模型即可，改配置一行。
- **DeepSeek 在这个网关上明显更慢**（5–12 秒），且思考 token 较多。
  两者并行，所以总耗时不叠加。

## 五、怎么复跑

```bash
export DEEPSEEK_API_KEY=<网关密钥>
export ANTHROPIC_API_KEY=<网关密钥>

go run ./cmd/eval -real -config config.example.json -out data/eval-real
go run ./cmd/rehearse -real -config config.example.json -out data/rehearsal-real
go run ./cmd/audit -case data/cases/C01 -config config.example.json
```

评测约 10 分钟（120 次模型调用）。跑完把 `-out` 目录里的 md 覆盖到 `docs/eval-real.md` 即可。

## 六、仍未验证的

1. **官方接口未测**：本次只验证了这个第三方网关。官方 DeepSeek 与 Anthropic 直连没有密钥，
   代码里两套协议都支持，但官方端点上的行为（尤其 `output_config` 是否被接受）未实测。
2. **真实票据未测**：60 条仍是合成票据，只是模型换成了真的。真实票据的版式更花，抽取准确率会下降。
3. **网关稳定性未知**：测试期间出现过偶发 401（同样请求重发即成功），
   代码的重试逻辑会兜住一次，但如果网关抖动频繁，评分期间可能变慢。
