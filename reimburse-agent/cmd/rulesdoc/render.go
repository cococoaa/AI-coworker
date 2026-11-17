package main

import (
	"fmt"
	"strings"

	"reimburse/plugins/reimburse-audit/policy"
)

func render(rf *policy.File, byCheck map[string][]string, withCases bool) string {
	var b strings.Builder
	p := rf.Params
	fmt.Fprintf(&b, "# %s\n\n版本：%s\n\n", rf.Policy.Name, rf.Policy.Version)
	fmt.Fprintf(&b, "> **声明**：%s\n\n", rf.Policy.Disclaimer)
	fmt.Fprintf(&b, "适用范围：%s\n\n", rf.Policy.Scope)
	b.WriteString("本文件由 `go run ./cmd/rulesdoc` 从 `rules/policy.json` 生成，请改规则表而不是改本文件。\n\n")

	b.WriteString("## 一、判定原则\n\n")
	for _, n := range rf.Policy.Notes {
		fmt.Fprintf(&b, "- %s\n", n)
	}
	fmt.Fprintf(&b, "\n结论优先级（从严到宽）：%s\n\n", names(outcomeName, p.DecisionPriority))

	b.WriteString("## 二、参数（虚构 mock 值）\n\n| 参数 | 值 |\n|---|---|\n")
	row := func(k, v string) { fmt.Fprintf(&b, "| %s | %s |\n", k, cell(v)) }
	row("公司", fmt.Sprintf("%s（税号 %s）", p.Company.Name, p.Company.TaxID))
	row("报销期限", fmt.Sprintf("费用发生后 %d 天内", p.ReimburseDeadlineDays))
	row("交通日期缓冲", fmt.Sprintf("出差期间前后各 %d 天", p.TransportDateBufferDays))
	row("金额容差", fmt.Sprintf("%d 分", p.AmountToleranceFen))
	row("字段低置信度线", fmt.Sprintf("%.2f，低于此值视为不可信", p.LowConfidenceThreshold))
	row("关键字段", strings.Join(p.KeyFields, "、"))
	row("招待人均上限", fmt.Sprintf("%d 元（工作餐 %d 元）", p.MealPerCapitaYuan.Entertain, p.MealPerCapitaYuan.WorkMeal))
	row("招待审批线", fmt.Sprintf("%d 元（达到即须事前审批号）", p.EntertainApprovalLineYuan))
	row("拆分报销窗口", fmt.Sprintf("前后 %d 天", p.SplitWindowDays))
	row("禁止项关键词", strings.Join(p.ForbiddenKeywords, "、"))
	row("一线城市", strings.Join(p.CityTiers.Tier1, "、"))
	row("二线城市", strings.Join(p.CityTiers.Tier2, "、"))
	row("三线城市", "其余城市")
	b.WriteString("\n职级：P 普通员工，M 经理，D 总监。\n\n")

	b.WriteString("### 住宿限额（元/晚）\n\n| 城市档次 | P | M | D |\n|---|---|---|---|\n")
	for _, t := range []string{"tier1", "tier2", "tier3"} {
		l := p.LodgingLimitYuan[t]
		fmt.Fprintf(&b, "| %s | %d | %d | %d |\n", map[string]string{"tier1": "一线", "tier2": "二线", "tier3": "三线"}[t], l["P"], l["M"], l["D"])
	}
	b.WriteString("\n### 允许席别\n\n| 职级 | 火车 | 飞机 |\n|---|---|---|\n")
	for _, g := range []string{"P", "M", "D"} {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", g, strings.Join(p.TrainSeats[g], "、"), strings.Join(p.FlightSeats[g], "、"))
	}

	b.WriteString("\n## 三、规则与判定项\n\n")
	for _, r := range rf.Rules {
		fmt.Fprintf(&b, "### %s %s（%s）\n\n", r.ID, r.Name, r.Clause)
		fmt.Fprintf(&b, "- 条款原文：%s\n- 适用类别：%s\n", r.ClauseText, names(catName, r.AppliesTo))
		if len(r.Tools) > 0 {
			fmt.Fprintf(&b, "- 依赖工具：%s\n", strings.Join(r.Tools, "、"))
		}
		fmt.Fprintf(&b, "- 出处类别：%s\n", strings.Join(r.Basis, "；"))
		if r.Note != "" {
			fmt.Fprintf(&b, "- 备注：%s\n", r.Note)
		}
		b.WriteString("\n| 判定项 | 触发条件 | 结论 |")
		if withCases {
			b.WriteString(" 测试用例 |")
		}
		b.WriteString("\n|---|---|---|")
		if withCases {
			b.WriteString("---|")
		}
		b.WriteString("\n")
		for _, c := range r.Checks {
			res := outcomeName[c.Outcome]
			if c.Adjust != "" {
				res += "（" + c.Adjust + "）"
			}
			fmt.Fprintf(&b, "| %s | %s | %s |", c.ID, cell(c.When), cell(res))
			if withCases {
				fmt.Fprintf(&b, " %s |", strings.Join(byCheck[c.ID], " "))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("## 四、不作判定的情形（避免用猜测值误判）\n\n")
	b.WriteString("某关键字段不可信时，依赖它的判定项不作判定，只由 R15-b 转人工；某工具失败或超时时同理，由 R15-a 转人工。\n\n")
	b.WriteString("| 不可信的字段 | 不作判定的判定项 |\n|---|---|\n")
	for _, k := range sortedKeys(p.FieldDeps) {
		fmt.Fprintf(&b, "| %s | %s |\n", k, strings.Join(p.FieldDeps[k], "、"))
	}
	b.WriteString("\n| 失败的工具 | 不作判定的判定项 |\n|---|---|\n")
	for _, k := range sortedKeys(p.ToolDeps) {
		fmt.Fprintf(&b, "| %s | %s |\n", k, strings.Join(p.ToolDeps[k], "、"))
	}
	return b.String()
}
