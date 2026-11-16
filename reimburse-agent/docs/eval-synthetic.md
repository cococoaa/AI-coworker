# 评测报告

生成时间：2026-11-16 17:56:35　模式：**合成抽取（无密钥演示模式）**

> 本次评测使用合成抽取（由测试集标准答案生成结构化输出，并经与真实响应完全相同的
> Schema 校验与双模型比对）。它验证的是**判定链路的正确性**，不代表任何真实模型的能力。
> 配好密钥后执行 `go run ./cmd/eval -real -out docs` 可得到真实模型评测。

## 总体指标

| 指标 | 数值 |
|---|---|
| 用例数 | 60 |
| 结论与标准答案一致 | 60 / 60（100.0%） |
| 命中项集合完全一致 | 60 / 60 |
| 判定项精确率 | 1.000 |
| 判定项召回率 | 1.000 |
| 判定项覆盖 | 26 / 26 |
| 核准金额一致 | 19 / 19 条（分母为通过/调整后通过的用例数） |
| 核准金额平均绝对误差 | 0 分 |
| 平均单条耗时 | 1.3 ms |
| 平均单条成本 | $0.000000 |

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

无。

## 逐条明细

| 用例 | 类别 | 期望 | 实际 | 命中项 | 事件 | 耗时 |
|---|---|---|---|---|---|---|
| C01 | transport | approve | approve |  | 28 | 3ms |
| C02 | transport | approve | approve |  | 28 | 1ms |
| C03 | lodging | approve | approve |  | 29 | 1ms |
| C04 | meal | approve | approve |  | 31 | 2ms |
| C05 | meal | approve | approve |  | 29 | 1ms |
| C06 | meal | approve | approve |  | 31 | 1ms |
| C07 | lodging | approve | approve |  | 30 | 1ms |
| C08 | lodging | approve_with_adjustment | approve_with_adjustment | R10-a | 31 | 2ms |
| C09 | meal | approve | approve |  | 29 | 1ms |
| C10 | transport | approve | approve |  | 28 | 1ms |
| C11 | transport | need_more_info | need_more_info | R07-a | 29 | 2ms |
| C12 | meal | approve | approve |  | 29 | 1ms |
| C13 | meal | need_more_info | need_more_info | R05-a | 31 | 2ms |
| C14 | transport | approve | approve |  | 28 | 0ms |
| C15 | transport | approve | approve |  | 27 | 2ms |
| C16 | meal | manual_review | manual_review | R05-b | 30 | 1ms |
| C17 | meal | approve | approve |  | 29 | 1ms |
| C18 | transport | approve | approve |  | 28 | 2ms |
| C19 | lodging | approve | approve |  | 30 | 1ms |
| C20 | transport | reject | reject | R01-a | 29 | 0ms |
| C21 | lodging | reject | reject | R01-a | 31 | 0ms |
| C22 | meal | reject | reject | R01-b | 33 | 0ms |
| C23 | transport | manual_review | manual_review | R01-c | 28 | 6ms |
| C24 | transport | reject | reject | R02-a R02-b | 30 | 1ms |
| C25 | lodging | reject | reject | R02-a | 31 | 1ms |
| C26 | meal | reject | reject | R02-b | 32 | 2ms |
| C27 | lodging | reject | reject | R03-a | 31 | 1ms |
| C28 | meal | reject | reject | R03-b | 30 | 1ms |
| C29 | lodging | reject | reject | R04-a | 31 | 2ms |
| C30 | transport | manual_review | manual_review | R04-b | 29 | 0ms |
| C31 | meal | manual_review | manual_review | R05-b | 30 | 2ms |
| C32 | meal | approve | approve |  | 29 | 1ms |
| C33 | lodging | reject | reject | R06-a | 31 | 2ms |
| C34 | transport | manual_review | manual_review | R06-b | 29 | 1ms |
| C35 | transport | manual_review | manual_review | R08-a | 29 | 1ms |
| C36 | lodging | manual_review | manual_review | R08-a | 31 | 2ms |
| C37 | transport | need_more_info | need_more_info | R09-a | 29 | 0ms |
| C38 | transport | manual_review | manual_review | R09-b | 29 | 2ms |
| C39 | meal | approve_with_adjustment | approve_with_adjustment | R11-a | 32 | 2ms |
| C40 | meal | approve_with_adjustment | approve_with_adjustment | R11-a | 30 | 1ms |
| C41 | transport | manual_review | manual_review | R12-a | 29 | 2ms |
| C42 | meal | reject | reject | R13-a | 32 | 0ms |
| C43 | lodging | manual_review | manual_review | R13-b | 31 | 1ms |
| C44 | transport | need_more_info | need_more_info | R14-a | 34 | 1ms |
| C45 | transport | need_more_info | need_more_info | R14-b | 29 | 0ms |
| C46 | meal | need_more_info | need_more_info | R14-b | 29 | 4ms |
| C47 | lodging | reject | reject | R01-a R02-a R13-a | 33 | 1ms |
| C48 | transport | need_more_info | need_more_info | R07-a R09-a R14-b | 31 | 2ms |
| C49 | lodging | manual_review | manual_review | R10-a R12-a | 32 | 1ms |
| C50 | meal | need_more_info | need_more_info | R05-a R11-a | 32 | 1ms |
| C51 | transport | manual_review | manual_review | R15-a | 32 | 1ms |
| C52 | lodging | manual_review | manual_review | R15-a | 34 | 1ms |
| C53 | meal | manual_review | manual_review | R15-a | 33 | 1ms |
| C54 | lodging | manual_review | manual_review | R15-a | 32 | 1ms |
| C55 | lodging | manual_review | manual_review | R15-b | 31 | 1ms |
| C56 | meal | manual_review | manual_review | R15-b | 33 | 2ms |
| C57 | transport | manual_review | manual_review | R15-b | 28 | 1ms |
| C58 | lodging | manual_review | manual_review | R15-b | 28 | 1ms |
| C59 | transport | manual_review | manual_review | R15-b | 30 | 1ms |
| C60 | meal | manual_review | manual_review | R15-a R15-b | 35 | 2ms |

## 这份报告不能说明什么

1. 不是真实票据上的效果：60 条用例是合成的，规则阈值也是虚构值。
2. 合成模式不代表模型能力：字段来自标准答案，模型真正的抽取错误率要等真实评测。
3. 不含真实税局查验与企业查询的时延与失败率。
