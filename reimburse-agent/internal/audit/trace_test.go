package audit

import (
	"strings"
	"testing"

	"reimburse/internal/llm"
	"reimburse/internal/schema"
	"reimburse/plugins/reimburse-audit/judge"
)

// traceCase 取一张真实用例，用它的发票原文做溯源测试。
// 刻意不用自己构造的文本：那样只能证明"我的匹配函数认识我自己写的东西"。
func traceCase(t *testing.T, id string) *schema.Case {
	t.Helper()
	p := "../../data/cases/" + id
	c, err := schema.LoadCase(p)
	if err != nil {
		t.Skipf("用例 %s 不可用: %v", id, err)
	}
	return c
}

// traceReport 只装一个抽取结果与字段状态，够 TraceFields 用。
func traceReport(fields map[string]llm.FieldValue, st judge.Statuses) *Report {
	d := &llm.DualResult{Fields: fields, BothOK: true}
	return &Report{Extract: d, Result: &Result{FieldStatus: st}}
}

// 溯源的核心承诺：抽出的值能回到原件里指出出处。
// 这里用真实用例的发票原文，确保不是自己构造的"刚好能匹配"的文本。
func TestTraceFieldsFindsRealValues(t *testing.T) {
	c := traceCase(t, "C01")
	rep := traceReport(map[string]llm.FieldValue{
		"invoice_no":    {Value: c.Truth.Invoice.InvoiceNo, Conf: 0.98, Presence: "present"},
		"issue_date":    {Value: c.Truth.Invoice.IssueDate, Conf: 0.98, Presence: "present"},
		"total_amount":  {Value: "553.00", Conf: 0.98, Presence: "present"},
		"seller_name":   {Value: c.Truth.Invoice.SellerName, Conf: 0.98, Presence: "present"},
		"seller_tax_id": {Value: c.Truth.Invoice.SellerTaxID, Conf: 0.98, Presence: "present"},
	}, judge.Statuses{})
	ts := TraceFields(rep, c.InvoiceText, c.SupportText)

	byField := map[string]FieldTrace{}
	for _, x := range ts {
		byField[x.Field] = x
	}

	// 发票号码、销售方、税号都应在发票原文里找到。
	for _, f := range []string{"invoice_no", "seller_tax_id", "seller_name"} {
		x := byField[f]
		if x.Source != TraceInvoice {
			t.Errorf("%s 应在发票原文中找到，实际 source=%s note=%s", f, x.Source, x.Note)
		}
		if x.Snippet == "" {
			t.Errorf("%s 应给出原文片段", f)
		}
	}

	// 每个字段都要出现，不能悄悄少一行——否则界面上会看起来"全部核对过了"。
	if len(ts) != len(judge.FieldNames()) {
		t.Errorf("应为全部 %d 个字段给出溯源，实际 %d 个", len(judge.FieldNames()), len(ts))
	}
}

// 原件里没有的值要标出来，而不是当成已核对。
func TestTraceFlagsValueNotInSource(t *testing.T) {
	c := traceCase(t, "C01")
	// 金额被模型改写成一个原件里不存在的数。
	rep := traceReport(map[string]llm.FieldValue{
		"total_amount": {Value: "999.00", Conf: 0.97, Presence: "present"},
	}, judge.Statuses{})
	ts := TraceFields(rep, c.InvoiceText, "")
	var found bool
	for _, x := range ts {
		if x.Field == "total_amount" {
			found = true
			if x.Source != TraceNone {
				t.Errorf("原件里没有 999.00，应标为未找到，实际 %s", x.Source)
			}
			if x.Note == "" {
				t.Error("未找到的字段必须给出说明")
			}
		}
	}
	if !found {
		t.Fatal("金额字段应出现在溯源结果里")
	}
}

// 空值与非空值的区分：absent 的字段不该被说成"未找到"。
func TestTraceEmptyValueIsNotSuspicious(t *testing.T) {
	rep := traceReport(map[string]llm.FieldValue{}, judge.Statuses{
		judge.FBuyerTaxID: judge.Absent,
	})
	ts := TraceFields(rep, "发票号码 123\n价税合计 100.00", "")
	for _, x := range ts {
		if x.Field != "buyer_tax_id" {
			continue
		}
		if x.Source != TraceEmpty {
			t.Errorf("原件没有该栏时应标为空值，实际 %s", x.Source)
		}
		if !strings.Contains(x.Note, "为空") {
			t.Errorf("应说明是原件为空而不是识别失败，实际 %q", x.Note)
		}
	}
}

// 只有佐证凭证里有、发票里没有：这是危险信号，必须单独标出。
func TestTraceSupportOnlyIsFlagged(t *testing.T) {
	rep := traceReport(map[string]llm.FieldValue{
		"issue_date": {Value: "2026-11-01", Conf: 0.95, Presence: "present"},
	}, judge.Statuses{})
	ts := TraceFields(rep, "发票号码 12345678\n价税合计 100.00", "行程日期 2026-11-01 上海虹桥→北京南")
	var got FieldTrace
	for _, x := range ts {
		if x.Field == "issue_date" {
			got = x
		}
	}
	if got.Source != TraceSupport {
		t.Errorf("应标为出自佐证凭证，实际 %s", got.Source)
	}
	if !strings.Contains(got.Note, "以发票为准") {
		t.Errorf("应提醒该字段以发票为准，实际 %q", got.Note)
	}
}

// 排版差异不应该被当成"找不到"：千分位、中文日期都要认。
func TestTraceAcceptsFormattingVariants(t *testing.T) {
	cases := []struct {
		name    string
		field   judge.FieldName
		value   string
		invoice string
	}{
		{"金额带千分位", judge.FTotalAmount, "1234.00", "价税合计（大写）壹仟贰佰叁拾肆元整  （小写）¥1,234.00"},
		{"金额省略小数", judge.FTotalAmount, "553.00", "价税合计 ￥553"},
		{"日期中文写法", judge.FIssueDate, "2026-11-01", "开票日期：2026年11月1日"},
		{"日期斜杠写法", judge.FIssueDate, "2026-11-01", "开票日期 2026/11/01"},
		{"中间有空格", judge.FSellerName, "星河科技有限公司", "销售方名称：星河 科技 有限公司"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := traceReport(map[string]llm.FieldValue{
				string(tc.field): {Value: tc.value, Conf: 0.95, Presence: "present"},
			}, judge.Statuses{})
			for _, x := range TraceFields(rep, tc.invoice, "") {
				if x.Field != string(tc.field) {
					continue
				}
				if x.Source != TraceInvoice {
					t.Errorf("排版差异不应导致找不到：%q 在 %q 中，实际 source=%s",
						tc.value, tc.invoice, x.Source)
				}
				return
			}
			t.Fatalf("未找到字段 %s", tc.field)
		})
	}
}

// 值改了就是改了：数字不同不能靠"排版放宽"蒙混过去。
func TestTraceRejectsDifferentNumber(t *testing.T) {
	rep := traceReport(map[string]llm.FieldValue{
		"total_amount": {Value: "1235.00", Conf: 0.95, Presence: "present"},
	}, judge.Statuses{})
	for _, x := range TraceFields(rep, "价税合计 ¥1,234.00", "") {
		if x.Field == "total_amount" && x.Source != TraceNone {
			t.Errorf("1235 与 1234 不同，不应判为找到，实际 %s", x.Source)
		}
	}
}
