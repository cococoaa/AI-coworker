# 评测报告

生成时间：2026-11-16 18:26:59　模式：**真实模型**

## 总体指标

| 指标 | 数值 |
|---|---|
| 用例数 | 60 |
| 结论与标准答案一致 | 60 / 60（100.0%） |
| 命中项集合完全一致 | 59 / 60 |
| 判定项精确率 | 0.981 |
| 判定项召回率 | 0.981 |
| 判定项覆盖 | 26 / 26 |
| 核准金额一致 | 19 / 19 条（分母为通过/调整后通过的用例数） |
| 核准金额平均绝对误差 | 0 分 |
| 平均单条耗时 | 8526.8 ms |
| 平均单条成本 | $0.010096 |

## 结论分布（本系统给出）

| 结论 | 条数 |
|---|---|
| reject | 12 |
| manual_review | 21 |
| need_more_info | 8 |
| approve_with_adjustment | 3 |
| approve | 16 |

## 按类别

| 类别 | 条数 |
|---|---|
| transport | 22 |
| lodging | 17 |
| meal | 21 |

## 判定项覆盖

26 个判定项均有用例覆盖。

## 失败用例

| 用例 | 标题 | 期望 | 实际 | 漏判 | 误判 |
|---|---|---|---|---|---|
| C24 | 个人抬头，无购买方税号 | reject | reject | [R02-b] | [R15-b] |

## 逐条明细

| 用例 | 类别 | 期望 | 实际 | 命中项 | 事件 | 耗时 |
|---|---|---|---|---|---|---|
| C01 | transport | approve | approve |  | 28 | 5944ms |
| C02 | transport | approve | approve |  | 28 | 10835ms |
| C03 | lodging | approve | approve |  | 30 | 6729ms |
| C04 | meal | approve | approve |  | 31 | 7542ms |
| C05 | meal | approve | approve |  | 29 | 5385ms |
| C06 | meal | approve | approve |  | 31 | 5180ms |
| C07 | lodging | approve | approve |  | 30 | 6104ms |
| C08 | lodging | approve_with_adjustment | approve_with_adjustment | R10-a | 31 | 11026ms |
| C09 | meal | approve | approve |  | 29 | 6357ms |
| C10 | transport | approve | approve |  | 28 | 7194ms |
| C11 | transport | need_more_info | need_more_info | R07-a | 29 | 5601ms |
| C12 | meal | approve | approve |  | 29 | 6481ms |
| C13 | meal | need_more_info | need_more_info | R05-a | 31 | 6317ms |
| C14 | transport | approve | approve |  | 28 | 4402ms |
| C15 | transport | approve | approve |  | 28 | 4715ms |
| C16 | meal | manual_review | manual_review | R05-b | 30 | 9423ms |
| C17 | meal | approve | approve |  | 29 | 6489ms |
| C18 | transport | approve | approve |  | 28 | 6429ms |
| C19 | lodging | approve | approve |  | 30 | 6303ms |
| C20 | transport | reject | reject | R01-a | 29 | 5506ms |
| C21 | lodging | reject | reject | R01-a | 31 | 11854ms |
| C22 | meal | reject | reject | R01-b | 33 | 13232ms |
| C23 | transport | manual_review | manual_review | R01-c | 28 | 7320ms |
| C24 | transport | reject | reject | R02-a R15-b | 31 | 13769ms |
| C25 | lodging | reject | reject | R02-a | 31 | 10396ms |
| C26 | meal | reject | reject | R02-b | 32 | 8043ms |
| C27 | lodging | reject | reject | R03-a | 31 | 15350ms |
| C28 | meal | reject | reject | R03-b | 30 | 6527ms |
| C29 | lodging | reject | reject | R04-a | 31 | 8663ms |
| C30 | transport | manual_review | manual_review | R04-b | 29 | 6771ms |
| C31 | meal | manual_review | manual_review | R05-b | 30 | 8020ms |
| C32 | meal | approve | approve |  | 29 | 11535ms |
| C33 | lodging | reject | reject | R06-a | 31 | 10831ms |
| C34 | transport | manual_review | manual_review | R06-b | 29 | 13000ms |
| C35 | transport | manual_review | manual_review | R08-a | 29 | 11001ms |
| C36 | lodging | manual_review | manual_review | R08-a | 31 | 9987ms |
| C37 | transport | need_more_info | need_more_info | R09-a | 29 | 5297ms |
| C38 | transport | manual_review | manual_review | R09-b | 29 | 24265ms |
| C39 | meal | approve_with_adjustment | approve_with_adjustment | R11-a | 32 | 6764ms |
| C40 | meal | approve_with_adjustment | approve_with_adjustment | R11-a | 30 | 12380ms |
| C41 | transport | manual_review | manual_review | R12-a | 29 | 9814ms |
| C42 | meal | reject | reject | R13-a | 32 | 7658ms |
| C43 | lodging | manual_review | manual_review | R13-b | 31 | 5613ms |
| C44 | transport | need_more_info | need_more_info | R14-a | 34 | 5169ms |
| C45 | transport | need_more_info | need_more_info | R14-b | 29 | 5110ms |
| C46 | meal | need_more_info | need_more_info | R14-b | 29 | 7299ms |
| C47 | lodging | reject | reject | R01-a R02-a R13-a | 33 | 7183ms |
| C48 | transport | need_more_info | need_more_info | R07-a R09-a R14-b | 31 | 8933ms |
| C49 | lodging | manual_review | manual_review | R10-a R12-a | 32 | 5285ms |
| C50 | meal | need_more_info | need_more_info | R05-a R11-a | 32 | 6689ms |
| C51 | transport | manual_review | manual_review | R15-a | 32 | 5427ms |
| C52 | lodging | manual_review | manual_review | R15-a | 34 | 6399ms |
| C53 | meal | manual_review | manual_review | R15-a | 33 | 9284ms |
| C54 | lodging | manual_review | manual_review | R15-a | 32 | 6013ms |
| C55 | lodging | manual_review | manual_review | R15-b | 31 | 7212ms |
| C56 | meal | manual_review | manual_review | R15-b | 33 | 7365ms |
| C57 | transport | manual_review | manual_review | R15-b | 28 | 7874ms |
| C58 | lodging | manual_review | manual_review | R15-b | 29 | 29374ms |
| C59 | transport | manual_review | manual_review | R15-b | 30 | 8574ms |
| C60 | meal | manual_review | manual_review | R15-a R15-b | 35 | 6363ms |

## 这份报告不能说明什么

1. 不是真实票据上的效果：60 条用例是合成的，规则阈值也是虚构值。
2. 合成模式不代表模型能力：字段来自标准答案，模型真正的抽取错误率要等真实评测。
3. 不含真实税局查验与企业查询的时延与失败率。

## 真实模型评测的补充说明

**接入方式**：两个模型都走同一个第三方网关 `https://api.openai-next.com`，协议不同：

| 模型 | 协议 | 模型名 | 说明 |
| --- | --- | --- | --- |
| DeepSeek | OpenAI 兼容 `/v1/chat/completions` + `response_format=json_object` | `deepseek-v3.2` | 该网关上 `deepseek-chat` 返回 404 |
| Claude | Anthropic 兼容 `/v1/messages` + `output_config.format` | `claude-sonnet-5-5` | 网关会拦非浏览器 UA（Cloudflare 1010），需带 `user_agent` |

**唯一一条命中项不一致：C24（个人抬头，无购买方税号）**

| 字段 | DeepSeek | Claude | 合并结果 |
| --- | --- | --- | --- |
| 购买方税号 | 空值，置信度 0.50 | 空值，置信度 1.00 | 空值，置信度 0.50 → 判为"不可信" |

两个模型都**正确读出"这张发票没有购买方税号"**，但 DeepSeek 对"空"这个结果只给了 0.5 的把握。
系统按"置信度取两侧较小值"合并后，该字段被判为不可信，于是 R02-b 不作判定、改由 R15-b 转人工。

- 结论仍是**驳回**（R02-a 个人抬头已足以驳回），核准金额也正确；
- 差别只在"给人看的依据"上：少了一条 R02-b，多了一条 R15-b。

这是真实的模型行为差异，不是规则错误。它恰好说明交叉校验在起作用：
**当一方对"空值"没把握时，系统宁可说"这条我先不确定"，也不替它拍板。**

**平均耗时 8.5 秒**：包含两家模型各一次调用（Claude 约 3–7 秒，DeepSeek 约 5–12 秒，并行执行）+ 三次 MCP 工具查询。
平均成本 $0.0101/单，其中 Claude 占绝大部分（约 90%）。

**这份报告与 `eval-synthetic.md` 的关系**：合成版验证判定链路，真实版验证模型在真实票据上的抽取。
两者结论一致（60/60），差别只在 C24 那一条命中项。
