package judge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"reimburse/plugins/reimburse-audit/policy"
)

// 插件侧的核心回归测试：判定实现必须与标准答案完全一致。
// 这里不调模型、不调工具，只读用例文件，所以跑得快（60 条 < 100ms）。
//
// 与 tools/conformance 的分工：那个是给 fork 者的命令行工具（输出人读报告），
// 这个是 CI 里跑的 go test。

func casesDir() string {
	if d := os.Getenv("REIMBURSE_CASES"); d != "" {
		return d
	}
	return "../fixtures/cases"
}

// fixtureCase 只取测试需要的最小结构。
type fixtureCase struct {
	ID    string
	Input *Input
	Truth struct {
		Decision    string   `json:"decision"`
		Hits        []string `json:"hits"`
		ApprovedFen int64    `json:"approved_fen"`
	}
}

// TestConformance 逐条比对判定结果与标准答案。
func TestConformance(t *testing.T) {
	f, err := loadFixturePolicy(t)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := loadFixtures(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 50 {
		t.Fatalf("用例太少（%d），检查路径 %s", len(cases), casesDir())
	}
	eng := New(f)
	for _, c := range cases {
		res := eng.Judge(c.Input)
		if res.Decision != c.Truth.Decision {
			t.Errorf("%s 结论 %s，期望 %s（命中 %v）", c.ID, res.Decision, c.Truth.Decision, res.Hits)
		}
		if !sameSet(res.Hits, c.Truth.Hits) {
			t.Errorf("%s 命中项 实际%v 期望%v", c.ID, res.Hits, c.Truth.Hits)
		}
		if res.ApprovedFen != c.Truth.ApprovedFen {
			t.Errorf("%s 核准金额 %d，期望 %d", c.ID, res.ApprovedFen, c.Truth.ApprovedFen)
		}
	}
}

// 每条命中都必须带依据，跳过的每条必须写原因——人工复核要知道"为什么"和"没查什么"。
func TestFindingsExplain(t *testing.T) {
	f, err := loadFixturePolicy(t)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := loadFixtures(t)
	if err != nil {
		t.Fatal(err)
	}
	eng := New(f)
	for _, c := range cases {
		res := eng.Judge(c.Input)
		for _, fd := range res.Findings {
			if fd.Reason == "" {
				t.Errorf("%s %s 命中但没写依据", c.ID, fd.CheckID)
			}
			if fd.Clause == "" || fd.ClauseText == "" {
				t.Errorf("%s %s 缺条款编号或原文", c.ID, fd.CheckID)
			}
		}
		for id, why := range res.Skipped {
			if why == "" {
				t.Errorf("%s %s 被跳过但没写原因", c.ID, id)
			}
		}
	}
}

// 规则表里的每个判定项都必须被代码处理：要么可能命中，要么会被记入 Skipped。
// 这条测试防的是"规则表加了规则、代码忘了实现"。
func TestEveryCheckHandled(t *testing.T) {
	f, err := loadFixturePolicy(t)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := loadFixtures(t)
	if err != nil {
		t.Fatal(err)
	}
	eng := New(f)
	seen := map[string]bool{}
	for _, c := range cases {
		res := eng.Judge(c.Input)
		for _, h := range res.Hits {
			seen[h] = true
		}
		for id := range res.Skipped {
			seen[id] = true
		}
	}
	for _, id := range f.AllCheckIDs() {
		if !seen[id] {
			t.Errorf("判定项 %s 从未被处理过（规则表有、代码没有）", id)
		}
	}
}

// 查无此票必须命中 R01-b 并驳回；超时必须转人工。
// 这条锁的是四态语义：NotFound 可用、Timeout 不可用。
func TestVerdictSemantics(t *testing.T) {
	if !Found.Usable() || !NotFound.Usable() {
		t.Fatal("查到与查无都应可用作判定依据")
	}
	if Failed.Usable() || Timeout.Usable() {
		t.Fatal("失败与超时都不可用作判定依据")
	}

	f, err := loadFixturePolicy(t)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := loadFixtures(t)
	if err != nil {
		t.Fatal(err)
	}
	eng := New(f)
	base := cases[0]

	// 构造：发票号可信，但查验返回"查无"
	in := cloneInput(base.Input)
	in.Tools["verify_invoice"] = ToolOutcome{Status: Verdict{Status: NotFound}}
	res := eng.Judge(in)
	if !hasHit(res.Hits, "R01-b") {
		t.Errorf("查无此票应命中 R01-b，实际 %v", res.Hits)
	}
	if res.Decision != "reject" {
		t.Errorf("查无此票应驳回，实际 %s", res.Decision)
	}

	// 构造：查验超时
	in = cloneInput(base.Input)
	in.Tools["verify_invoice"] = ToolOutcome{Status: Verdict{Status: Timeout, Reason: "timeout"}}
	res = eng.Judge(in)
	if hasHit(res.Hits, "R01-a") || hasHit(res.Hits, "R01-b") {
		t.Errorf("超时不得产生 R01 判定，实际 %v", res.Hits)
	}
	if res.Decision == "approve" {
		t.Error("查验超时不得通过")
	}
}

// 两个模型不一致的字段必须作废，不能任选一边。
func TestUntrustedFieldDropped(t *testing.T) {
	f, err := loadFixturePolicy(t)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := loadFixtures(t)
	if err != nil {
		t.Fatal(err)
	}
	eng := New(f)
	in := cloneInput(cases[0].Input)
	in.Untrusted[FTotalAmount] = true
	in.Fields[FTotalAmount] = Field{Value: "", Conf: 0}
	res := eng.Judge(in)
	if hasHit(res.Hits, "R06-a") {
		t.Error("金额不可信时不应据此判 R06-a")
	}
	if res.Decision == "approve" {
		t.Error("关键字段不可信时不得通过")
	}
}

// 单模型可用（无交叉校验）必须转人工。
func TestNoCrossCheckGoesManual(t *testing.T) {
	f, err := loadFixturePolicy(t)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := loadFixtures(t)
	if err != nil {
		t.Fatal(err)
	}
	eng := New(f)
	in := cloneInput(cases[0].Input)
	in.LowConfidence = append(in.LowConfidence, FieldName(LowConfidenceNoCrossCheck))
	res := eng.Judge(in)
	if !hasHit(res.Hits, "R15-b") {
		t.Errorf("无交叉校验应命中 R15-b，实际 %v", res.Hits)
	}
	if res.Decision == "approve" {
		t.Error("无交叉校验不得通过")
	}
}

// ---- 辅助 ----

func loadFixturePolicy(t *testing.T) (*policy.File, error) {
	t.Helper()
	return policy.LoadFile("../policy/policy.json")
}

// loadFixtures 把用例目录转成判定输入（与线上翻译逻辑一致）。
func loadFixtures(t *testing.T) ([]*fixtureCase, error) {
	t.Helper()
	dir := casesDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*fixtureCase
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		c, err := loadFixture(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func loadFixture(dir string) (*fixtureCase, error) {
	c := &fixtureCase{ID: filepath.Base(dir)}
	read := func(name string, v any) error {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		return json.Unmarshal(b, v)
	}
	var raw struct {
		ClaimNo   string `json:"claim_no"`
		RequestNo string `json:"request_no"`
		Applicant struct {
			Name       string `json:"name"`
			EmployeeID string `json:"employee_id"`
			Grade      string `json:"grade"`
		} `json:"applicant"`
		Category            string `json:"category"`
		City                string `json:"city"`
		StartDate           string `json:"start_date"`
		EndDate             string `json:"end_date"`
		Purpose             string `json:"purpose"`
		AmountFen           int64  `json:"amount_fen"`
		SubmitDate          string `json:"submit_date"`
		InternalCount       int    `json:"internal_count"`
		ExternalCount       int    `json:"external_count"`
		ExternalOrg         string `json:"external_org"`
		EntertainApprovalNo string `json:"entertain_approval_no"`
		SpecialApprovalNo   string `json:"special_approval_no"`
		Attachments         struct {
			SupportFile string `json:"support_file"`
		} `json:"attachments"`
	}
	if err := read("claim.json", &raw); err != nil {
		return nil, err
	}
	claim := &Claim{ClaimNo: raw.ClaimNo, RequestNo: raw.RequestNo,
		Applicant: Applicant{Name: raw.Applicant.Name, EmployeeID: raw.Applicant.EmployeeID, Grade: raw.Applicant.Grade},
		Category:  raw.Category, City: raw.City, StartDate: raw.StartDate, EndDate: raw.EndDate,
		Purpose: raw.Purpose, AmountFen: raw.AmountFen, SubmitDate: raw.SubmitDate,
		InternalCount: raw.InternalCount, ExternalCount: raw.ExternalCount, ExternalOrg: raw.ExternalOrg,
		EntertainApprovalNo: raw.EntertainApprovalNo, SpecialApprovalNo: raw.SpecialApprovalNo,
		SupportFile: raw.Attachments.SupportFile}

	var truth struct {
		Decision    string   `json:"decision"`
		Hits        []string `json:"hits"`
		ApprovedFen int64    `json:"approved_fen"`
		Invoice     struct {
			InvoiceNo   string `json:"invoice_no"`
			IssueDate   string `json:"issue_date"`
			BuyerName   string `json:"buyer_name"`
			BuyerTaxID  string `json:"buyer_tax_id"`
			SellerName  string `json:"seller_name"`
			SellerTaxID string `json:"seller_tax_id"`
			ItemName    string `json:"item_name"`
			TotalFen    int64  `json:"total_fen"`
		} `json:"invoice"`
		Unreadable []string `json:"unreadable"`
	}
	if err := read("truth.json", &truth); err != nil {
		return nil, err
	}
	c.Truth.Decision, c.Truth.Hits, c.Truth.ApprovedFen = truth.Decision, truth.Hits, truth.ApprovedFen

	var world struct {
		Invoices []struct {
			InvoiceNo   string `json:"invoice_no"`
			IssueDate   string `json:"issue_date"`
			TotalFen    int64  `json:"total_fen"`
			BuyerTaxID  string `json:"buyer_tax_id"`
			SellerTaxID string `json:"seller_tax_id"`
			Status      string `json:"status"`
		} `json:"invoices"`
		Companies []struct {
			TaxID  string `json:"tax_id"`
			Name   string `json:"name"`
			City   string `json:"city"`
			Status string `json:"status"`
		} `json:"companies"`
		History []struct {
			ClaimNo     string `json:"claim_no"`
			InvoiceNo   string `json:"invoice_no"`
			ApplicantID string `json:"applicant_id"`
			SellerTaxID string `json:"seller_tax_id"`
			IssueDate   string `json:"issue_date"`
			AmountFen   int64  `json:"amount_fen"`
			Category    string `json:"category"`
			Status      string `json:"status"`
			SubmitDate  string `json:"submit_date"`
		} `json:"history"`
		Faults []struct {
			Tool string `json:"tool"`
			Key  string `json:"key"`
			Mode string `json:"mode"`
		} `json:"faults"`
	}
	if err := read("world.json", &world); err != nil {
		return nil, err
	}

	// 字段：污损的置空 + 低置信度
	un := map[string]bool{}
	for _, f := range truth.Unreadable {
		un[f] = true
	}
	fields := Fields{}
	set := func(name FieldName, v string) {
		if un[string(name)] {
			fields[name] = Field{Value: "", Conf: 0.2}
			return
		}
		fields[name] = Field{Value: v, Conf: 1}
	}
	inv := truth.Invoice
	set(FInvoiceNo, inv.InvoiceNo)
	set(FIssueDate, inv.IssueDate)
	set(FTotalAmount, fenStr(inv.TotalFen))
	set(FBuyerTaxID, inv.BuyerTaxID)
	set(FSellerTaxID, inv.SellerTaxID)
	set(FBuyerName, inv.BuyerName)
	set(FSellerName, inv.SellerName)
	set(FItemName, inv.ItemName)

	untrusted := map[FieldName]bool{}
	var low []FieldName
	for k := range un {
		untrusted[FieldName(k)] = true
		low = append(low, FieldName(k))
	}

	fault := func(tool string) (Status, bool) {
		for _, f := range world.Faults {
			k := inv.InvoiceNo
			if f.Tool == "check_company" {
				k = inv.SellerTaxID
			}
			if f.Tool == tool && f.Key == k {
				if f.Mode == "timeout" {
					return Timeout, true
				}
				return Failed, true
			}
		}
		return 0, false
	}

	tools := map[string]ToolOutcome{}
	if st, bad := fault("verify_invoice"); bad {
		tools["verify_invoice"] = ToolOutcome{Status: Verdict{Status: st, Reason: "预置故障"}}
	} else {
		var recs []InvoiceRecord
		for _, r := range world.Invoices {
			if r.InvoiceNo == inv.InvoiceNo {
				recs = append(recs, InvoiceRecord{InvoiceNo: r.InvoiceNo, IssueDate: r.IssueDate,
					TotalFen: r.TotalFen, BuyerTaxID: r.BuyerTaxID, SellerTaxID: r.SellerTaxID,
					PlatformStatus: r.Status})
			}
		}
		st := NotFound
		if len(recs) > 0 {
			st = Found
		}
		tools["verify_invoice"] = ToolOutcome{Status: Verdict{Status: st}, Invoices: recs}
	}
	if st, bad := fault("check_company"); bad {
		tools["check_company"] = ToolOutcome{Status: Verdict{Status: st, Reason: "预置故障"}}
	} else {
		var recs []CompanyRecord
		for _, co := range world.Companies {
			if co.TaxID == inv.SellerTaxID {
				recs = append(recs, CompanyRecord{TaxID: co.TaxID, Name: co.Name, City: co.City, RegStatus: co.Status})
			}
		}
		st := NotFound
		if len(recs) > 0 {
			st = Found
		}
		tools["check_company"] = ToolOutcome{Status: Verdict{Status: st}, Companies: recs}
	}
	if st, bad := fault("check_history"); bad {
		tools["check_history"] = ToolOutcome{Status: Verdict{Status: st, Reason: "预置故障"}}
	} else {
		var recs []HistoryRecord
		for _, h := range world.History {
			recs = append(recs, HistoryRecord{ClaimNo: h.ClaimNo, InvoiceNo: h.InvoiceNo,
				ApplicantID: h.ApplicantID, SellerTaxID: h.SellerTaxID, IssueDate: h.IssueDate,
				AmountFen: h.AmountFen, Category: h.Category, RecStatus: h.Status, SubmitDate: h.SubmitDate})
		}
		tools["check_history"] = ToolOutcome{Status: Verdict{Status: Found}, History: recs}
	}

	var support string
	if raw.Attachments.SupportFile != "" {
		if b, err := os.ReadFile(filepath.Join(dir, raw.Attachments.SupportFile)); err == nil {
			support = string(b)
		}
	}

	c.Input = &Input{Claim: claim, Fields: fields, Untrusted: untrusted, LowConfidence: low,
		SupportText: support, Tools: tools}
	return c, nil
}

func cloneInput(in *Input) *Input {
	out := *in
	out.Fields = Fields{}
	for k, v := range in.Fields {
		out.Fields[k] = v
	}
	out.Untrusted = map[FieldName]bool{}
	for k, v := range in.Untrusted {
		out.Untrusted[k] = v
	}
	out.LowConfidence = append([]FieldName{}, in.LowConfidence...)
	out.Tools = map[string]ToolOutcome{}
	for k, v := range in.Tools {
		out.Tools[k] = v
	}
	return &out
}

func hasHit(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
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

func fenStr(fen int64) string {
	s := ""
	if fen < 0 {
		s, fen = "-", -fen
	}
	return s + itoa(fen/100) + "." + pad2(fen%100)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func pad2(n int64) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}
