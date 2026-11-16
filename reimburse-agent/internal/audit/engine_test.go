package audit

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"reimburse/internal/llm"
	"reimburse/internal/schema"
	"reimburse/rules"
)

// 这个测试是整个项目的核心回归断言：
// 引擎只读"从文本中抽取出来的字段 + 工具返回结果"，与 mockgen.Label
// "只读结构化真值"是两条独立的实现路径。60 条用例上两者的命中项集合、
// 结论、核准金额必须完全一致；任何一条不一致，说明引擎或标准答案至少一边写错了。

func casesDir() string {
	if d := os.Getenv("REIMBURSE_CASES"); d != "" {
		return d
	}
	return "../../data"
}

func loadCases() ([]*schema.Case, error) { return schema.LoadAll(casesDir()) }

func TestEngineMatchesLabelOnAllCases(t *testing.T) {
	rf, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	cases, err := loadCases()
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("没有读到用例")
	}
	eng := New(rf)
	for _, c := range cases {
		in := InputFromCase(c)
		got := eng.Judge(in)
		want := c.Truth.Hits
		if !sameSet(got.Hits, want) {
			t.Errorf("%s %s: 命中项不一致\n  引擎: %v\n  标注: %v\n  跳过: %v",
				c.ID, c.Truth.Title, got.Hits, want, skipSummary(got.Skipped))
			continue
		}
		if got.Decision != c.Truth.Decision {
			t.Errorf("%s: 结论不一致 引擎=%s 标注=%s", c.ID, got.Decision, c.Truth.Decision)
		}
		if got.ApprovedFen != c.Truth.ApprovedFen {
			t.Errorf("%s: 核准金额不一致 引擎=%d 标注=%d", c.ID, got.ApprovedFen, c.Truth.ApprovedFen)
		}
	}
}

// InputFromCase 把一条用例转换成引擎输入：模拟"模型已正确抽取"和"工具已正确返回"。
// unreadable 字段按模型抽不到处理（值为空、置信度 0）；
// faults 按对应工具的状态标记；查无此票按成功但无记录处理。
func InputFromCase(c *schema.Case) *Input {
	inv := c.Truth.Invoice
	un := map[string]bool{}
	for _, f := range c.Truth.Unreadable {
		un[f] = true
	}
	fields := map[string]llm.FieldValue{}
	set := func(name, v string) {
		if un[name] {
			fields[name] = llm.FieldValue{Value: "", Conf: 0}
			return
		}
		fields[name] = llm.FieldValue{Value: v, Conf: 1}
	}
	set(llm.FInvoiceNo, inv.InvoiceNo)
	set(llm.FIssueDate, inv.IssueDate)
	set(llm.FTotalAmount, fmt.Sprintf("%d.%02d", inv.TotalFen/100, inv.TotalFen%100))
	set(llm.FBuyerTaxID, inv.BuyerTaxID)
	set(llm.FSellerTaxID, inv.SellerTaxID)
	set(llm.FBuyerName, inv.BuyerName)
	set(llm.FSellerName, inv.SellerName)
	set(llm.FItemName, inv.ItemName)

	in := &Input{Claim: &c.Claim, Fields: fields, SupportText: c.SupportText, Tools: map[string]ToolResult{}}

	fault := func(tool, key string) ToolStatus {
		for _, f := range c.World.Faults {
			k := inv.InvoiceNo
			if f.Tool == "check_company" {
				k = inv.SellerTaxID
			}
			if f.Tool == tool && f.Key == k {
				if f.Mode == schema.FaultTimeout {
					return ToolTimeout
				}
				return ToolFailed
			}
		}
		return ""
	}
	// verify_invoice：必须在发票号可信时才会被调用。
	if st := fault("verify_invoice", ""); st != "" {
		in.Tools["verify_invoice"] = ToolResult{Status: st, Detail: "注入故障"}
	} else if !un[llm.FInvoiceNo] {
		in.Tools["verify_invoice"] = ToolResult{Status: ToolOK, Invoices: c.World.Invoices}
	}
	if st := fault("check_company", ""); st != "" {
		in.Tools["check_company"] = ToolResult{Status: st, Detail: "注入故障"}
	} else if !un[llm.FSellerTaxID] {
		in.Tools["check_company"] = ToolResult{Status: ToolOK, Companies: c.World.Companies}
	}
	if st := fault("check_history", ""); st != "" {
		in.Tools["check_history"] = ToolResult{Status: st, Detail: "注入故障"}
	} else if !un[llm.FInvoiceNo] {
		in.Tools["check_history"] = ToolResult{Status: ToolOK, History: c.World.History}
	}
	return in
}

func sameSet(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	sa, sb := map[string]bool{}, map[string]bool{}
	for _, x := range a {
		sa[x] = true
	}
	for _, x := range b {
		sb[x] = true
	}
	if len(sa) != len(sb) {
		return false
	}
	for k := range sa {
		if !sb[k] {
			return false
		}
	}
	return true
}

// skipSummary 只打印前几条跳过原因，便于定位。
func skipSummary(m map[string]string) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	if len(ks) == 0 {
		return "无"
	}
	var b strings.Builder
	for i, k := range ks {
		if i >= 3 {
			b.WriteString(" …")
			break
		}
		fmt.Fprintf(&b, "%s=%s; ", k, m[k])
	}
	return b.String()
}
