package audit_test

import (
	"context"
	"strings"
	"testing"

	"reimburse/internal/audit"
	"reimburse/internal/llm"
	"reimburse/internal/mcptools"
	"reimburse/internal/schema"
	"reimburse/rules"
)

// 外部输入鲁棒性：真实场景里会碰到空发票、超长文本、全角数字、字段缺失、日期倒挂等。
// 要求只有两条：
//  1. 不能 panic——服务端崩一次，整场演示就结束了；
//  2. 不能给出"通过"——看不懂的单据只能是转人工或补材料，不能猜。
func TestRobustnessOnBadInputs(t *testing.T) {
	rf, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	base := func() *schema.Claim {
		return &schema.Claim{
			ClaimNo: "BX-TEST", RequestNo: "SQ-TEST",
			Applicant: schema.Applicant{Name: "陈思远", EmployeeID: "E1001", Grade: "P"},
			Category:  "transport", City: "宁波", StartDate: "2026-10-01", EndDate: "2026-10-03",
			Purpose: "测试", AmountFen: 55300, SubmitDate: "2026-10-08",
			Attachments: schema.Attachments{InvoiceFile: "invoice.txt"},
		}
	}

	cases := []struct {
		name    string
		claim   *schema.Claim
		invoice string
		support string
	}{
		{"空发票文本", base(), "", ""},
		{"发票全是乱码", base(), strings.Repeat("▚▞░▒█%@#&", 200), ""},
		{"发票只有标题", base(), "电子发票（普通发票）\n", ""},
		{"超长发票文本", base(), strings.Repeat("发票号码：123\n开票日期：2026-10-01\n", 2000), ""},
		{"金额是中文大写", base(), "发票号码：26123456789012345678\n价税合计（小写）¥伍佰伍拾叁元\n", ""},
		{"日期倒挂", base(), "发票号码：26123456789012345678\n开票日期：2026-12-31\n", ""},
		{"申请金额为零", func() *schema.Claim { c := base(); c.AmountFen = 0; return c }(), "", ""},
		{"申请金额为负", func() *schema.Claim { c := base(); c.AmountFen = -1; return c }(), "", ""},
		{"类别未知", func() *schema.Claim { c := base(); c.Category = "unknown"; return c }(), "电子发票\n", ""},
		{"起止日期为空", func() *schema.Claim { c := base(); c.StartDate = ""; c.EndDate = ""; return c }(), "电子发票\n", ""},
		{"佐证凭证是乱码", base(), "电子发票\n", strings.Repeat("▚▞░▒", 500)},
		{"人数为负", func() *schema.Claim {
			c := base()
			c.Category, c.InternalCount, c.ExternalCount = "meal", -3, -2
			return c
		}(), "电子发票\n", ""},
	}

	cfg := llm.DefaultConfig()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("输入 %q 导致 panic：%v", c.name, r)
				}
			}()
			rv := &audit.Reviewer{LLM: &llm.SyntheticLLM{Case: &schema.Case{Claim: *c.claim},
				Synth: &llm.Synthetic{}}, Cfg: &cfg}
			// 空库：所有外部事实都查不到。
			srv := mcptools.NewServer(mcptools.Options{Data: mcptools.NewStore(schema.World{})})
			_, remote := mcptools.InMemory(t, srv)
			rep := audit.New(rf).Run(context.Background(), c.claim, c.invoice, c.support,
				audit.WrapMCP(remote), rv, audit.Options{})
			if rep == nil || rep.Result == nil {
				t.Fatal("没有返回结果")
			}
			if rep.Result.Decision == rules.Approve {
				t.Errorf("看不懂的输入不得通过，实际 %s，命中 %v", rep.Result.Decision, rep.Result.Hits)
			}
			if rep.Opinion == "" {
				t.Error("审核意见不应为空")
			}
		})
	}
}

// 工具返回脏数据（重复发票号、空字段、超大历史）时不能崩。
func TestRobustnessOnDirtyToolData(t *testing.T) {
	rf, _ := rules.Load()
	c := &schema.Claim{ClaimNo: "BX-1", RequestNo: "SQ-1",
		Applicant: schema.Applicant{Name: "陈思远", EmployeeID: "E1001", Grade: "P"},
		Category:  "transport", City: "宁波", StartDate: "2026-10-01", EndDate: "2026-10-03",
		AmountFen: 100, SubmitDate: "2026-10-04", Attachments: schema.Attachments{InvoiceFile: "i"}}

	var hist []schema.HistRecord
	for i := 0; i < 500; i++ {
		hist = append(hist, schema.HistRecord{ClaimNo: "H" + string(rune('A'+i%26)), InvoiceNo: "X",
			ApplicantID: "E1001", Status: schema.HistApproved})
	}
	w := schema.World{
		Invoices:  []schema.RegInvoice{{InvoiceNo: "", Status: ""}, {InvoiceNo: "DUP"}, {InvoiceNo: "DUP"}},
		Companies: []schema.Company{{TaxID: "", Status: "未知状态"}},
		History:   hist,
	}
	cfg := llm.DefaultConfig()
	rv := &audit.Reviewer{LLM: &llm.SyntheticLLM{Case: &schema.Case{Claim: *c}, Synth: &llm.Synthetic{}}, Cfg: &cfg}
	srv := mcptools.NewServer(mcptools.Options{Data: mcptools.NewStore(w)})
	_, remote := mcptools.InMemory(t, srv)
	rep := audit.New(rf).Run(context.Background(), c, "发票\n", "",
		audit.WrapMCP(remote), rv, audit.Options{})
	if rep == nil {
		t.Fatal("脏数据导致没有返回")
	}
	if rep.Result.Decision == rules.Approve {
		t.Error("工具数据不可信时不得通过")
	}
}
