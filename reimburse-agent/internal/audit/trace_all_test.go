package audit

import (
	"strings"
	"testing"

	"reimburse/internal/llm"
	"reimburse/internal/schema"
	"reimburse/plugins/reimburse-audit/judge"
)

// 全部 60 条回归：用标准答案当抽取结果，溯源不应把正确答案误判成"找不到"。
//
// 例外是发票本身被污损的用例：原件那一栏是 ████，标准答案里的值不可能出现在原文里。
// 这正是溯源要抓的情况——如果模型在这种情况下报出了一个完整值，那才是编的。
// 所以这里断言：凡判为"未找到"的，必须是发票上确实有遮挡的那几条。
func TestTraceOnAllCases(t *testing.T) {
	cs, err := schema.LoadAll("../../data")
	if err != nil {
		t.Fatal(err)
	}
	bad := 0
	total := 0
	var badCases []string
	for _, c := range cs {
		inv := c.Truth.Invoice
		rep := traceReport(map[string]llm.FieldValue{
			"invoice_no":    {Value: inv.InvoiceNo, Conf: 0.99, Presence: "present"},
			"issue_date":    {Value: inv.IssueDate, Conf: 0.99, Presence: "present"},
			"seller_name":   {Value: inv.SellerName, Conf: 0.99, Presence: "present"},
			"seller_tax_id": {Value: inv.SellerTaxID, Conf: 0.99, Presence: "present"},
		}, judge.Statuses{})
		for _, x := range TraceFields(rep, c.InvoiceText, c.SupportText) {
			if x.Value == "" {
				continue
			}
			total++
			if x.Source != TraceInvoice {
				bad++
				badCases = append(badCases, c.ID)
				t.Logf("%s %s(%s)=%q source=%s", c.ID, x.Field, x.Label, x.Value, x.Source)
			}
		}
	}
	t.Logf("共 %d 个有值字段，其中 %d 个在发票原文中未找到（%.1f%%）", total, bad, float64(bad)*100/float64(total))

	// 每一条"未找到"都要有污损作为解释，否则就是溯源误报。
	for _, id := range badCases {
		c, err := schema.LoadCase("../../data/cases/" + id)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(c.InvoiceText, "█") {
			t.Errorf("%s 被标为未找到，但发票原文没有遮挡字符——属于溯源误报", id)
		}
	}
	if bad > 5 {
		t.Errorf("未找到的字段过多（%d 个），匹配逻辑可能失效", bad)
	}
}
