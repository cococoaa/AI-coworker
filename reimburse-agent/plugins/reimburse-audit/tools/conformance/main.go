// conformance 校验"判定实现"与"标准答案"是否一致。
//
//	go run ./plugins/reimburse-audit/tools/conformance
//	go run ./plugins/reimburse-audit/tools/conformance -policy <path> -cases <dir> -verbose
//
// 它不调模型、不调工具：直接读每条用例的 truth.json 与 world.json，
// 把真值翻译成判定输入，跑一遍 judge，再逐项对账结论、命中项、核准金额。
//
// 这是 fork 者改完判定逻辑后的第一道验收：改坏了哪一条，它直接报用例编号与具体差异。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"reimburse/plugins/reimburse-audit/judge"
	"reimburse/plugins/reimburse-audit/policy"
)

func main() {
	policyPath := flag.String("policy", "plugins/reimburse-audit/policy/policy.json", "制度数据")
	casesDir := flag.String("cases", "plugins/reimburse-audit/fixtures/cases", "用例目录")
	verbose := flag.Bool("verbose", false, "打印每条用例的结果")
	flag.Parse()

	f, err := policy.LoadFile(*policyPath)
	if err != nil {
		fail("加载制度数据: %v", err)
	}
	entries, err := os.ReadDir(*casesDir)
	if err != nil {
		fail("读取用例目录: %v", err)
	}

	eng := judge.New(f)
	total, pass := 0, 0
	var failures []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		c, err := loadCase(filepath.Join(*casesDir, e.Name()))
		if err != nil {
			fail("%s: %v", e.Name(), err)
		}
		total++
		in := toInput(c)
		if *verbose && c.ID == "C22" {
			v := in.Tools["verify_invoice"]
			fmt.Printf("DEBUG C22 status=%v records=%d invNo=%q\n", v.Status.Status, len(v.Invoices), in.Fields[judge.FInvoiceNo].Value)
		}
		res := eng.Judge(in)
		ok := res.Decision == c.Truth.Decision &&
			sameSet(res.Hits, c.Truth.Hits) &&
			res.ApprovedFen == c.Truth.ApprovedFen
		if ok {
			pass++
		} else {
			failures = append(failures, fmt.Sprintf(
				"%s %s\n      结论 %s/%s  命中 期望%v 实际%v  金额 期望%d 实际%d",
				c.ID, c.Truth.Title, c.Truth.Decision, res.Decision,
				c.Truth.Hits, res.Hits, c.Truth.ApprovedFen, res.ApprovedFen))
		}
		if *verbose {
			fmt.Printf("%s %-40s %s\n", c.ID, c.Truth.Title, res.Decision)
		}
	}

	fmt.Printf("\n一致性：%d/%d 通过\n", pass, total)
	if len(failures) > 0 {
		fmt.Printf("\n不一致 %d 条：\n", len(failures))
		for _, s := range failures {
			fmt.Println("  ✗", s)
		}
		os.Exit(1)
	}
	fmt.Println("✓ 判定实现与标准答案完全一致")
}

// ---- 用例读取 ----

type testCase struct {
	ID      string
	Claim   judge.Claim
	Support string
	Truth   struct {
		Title       string   `json:"title"`
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
	World struct {
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
}

func loadCase(dir string) (*testCase, error) {
	c := &testCase{ID: filepath.Base(dir)}
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
		FromCity            string `json:"from_city"`
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
	c.Claim = judge.Claim{
		ClaimNo: raw.ClaimNo, RequestNo: raw.RequestNo,
		Applicant: judge.Applicant{Name: raw.Applicant.Name, EmployeeID: raw.Applicant.EmployeeID, Grade: raw.Applicant.Grade},
		Category:  raw.Category, FromCity: raw.FromCity, City: raw.City,
		StartDate: raw.StartDate, EndDate: raw.EndDate, Purpose: raw.Purpose,
		AmountFen: raw.AmountFen, SubmitDate: raw.SubmitDate,
		InternalCount: raw.InternalCount, ExternalCount: raw.ExternalCount, ExternalOrg: raw.ExternalOrg,
		EntertainApprovalNo: raw.EntertainApprovalNo, SpecialApprovalNo: raw.SpecialApprovalNo,
		SupportFile: raw.Attachments.SupportFile,
	}
	if raw.Attachments.SupportFile != "" {
		b, err := os.ReadFile(filepath.Join(dir, raw.Attachments.SupportFile))
		if err == nil {
			c.Support = string(b)
		}
	}
	if err := read("truth.json", &c.Truth); err != nil {
		return nil, err
	}
	if err := read("world.json", &c.World); err != nil {
		return nil, err
	}
	return c, nil
}

// toInput 把用例真值翻译成判定输入。
// 与线上一致：字段置信度 1.0（除非标为污损），工具按 world 返回，故障按 faults 标记。
func toInput(c *testCase) *judge.Input {
	un := map[string]bool{}
	for _, f := range c.Truth.Unreadable {
		un[f] = true
	}
	fields := judge.Fields{}
	set := func(name judge.FieldName, v string) {
		if un[string(name)] {
			fields[name] = judge.Field{Value: "", Conf: 0.2}
			return
		}
		fields[name] = judge.Field{Value: v, Conf: 1}
	}
	inv := c.Truth.Invoice
	set(judge.FInvoiceNo, inv.InvoiceNo)
	set(judge.FIssueDate, inv.IssueDate)
	set(judge.FTotalAmount, fmt.Sprintf("%d.%02d", inv.TotalFen/100, inv.TotalFen%100))
	set(judge.FBuyerTaxID, inv.BuyerTaxID)
	set(judge.FSellerTaxID, inv.SellerTaxID)
	set(judge.FBuyerName, inv.BuyerName)
	set(judge.FSellerName, inv.SellerName)
	set(judge.FItemName, inv.ItemName)

	untrusted := map[judge.FieldName]bool{}
	for k := range un {
		untrusted[judge.FieldName(k)] = true
	}
	var low []judge.FieldName
	for k := range un {
		low = append(low, judge.FieldName(k))
	}
	sort.Slice(low, func(i, j int) bool { return low[i] < low[j] })

	fault := func(tool, key string) judge.Status {
		for _, f := range c.World.Faults {
			k := inv.InvoiceNo
			if f.Tool == "check_company" {
				k = inv.SellerTaxID
			}
			if f.Tool == tool && f.Key == k {
				if f.Mode == "timeout" {
					return judge.Timeout
				}
				return judge.Failed
			}
		}
		return 0
	}

	tools := map[string]judge.ToolOutcome{}
	// verify_invoice
	if st := fault("verify_invoice", ""); st != 0 {
		tools["verify_invoice"] = judge.ToolOutcome{Status: judge.Verdict{Status: st, Reason: "预置故障"}}
	} else {
		var recs []judge.InvoiceRecord
		for _, r := range c.World.Invoices {
			if r.InvoiceNo == inv.InvoiceNo {
				recs = append(recs, judge.InvoiceRecord{InvoiceNo: r.InvoiceNo, IssueDate: r.IssueDate,
					TotalFen: r.TotalFen, BuyerTaxID: r.BuyerTaxID, SellerTaxID: r.SellerTaxID,
					PlatformStatus: r.Status})
			}
		}
		// 注意：登记库为空时要返回 NotFound（"查无此票"是确定事实），
		// 而不是返回 Found + 空列表——否则 R01-b 永远不会命中。
		st := judge.NotFound
		if len(recs) > 0 {
			st = judge.Found
		}
		tools["verify_invoice"] = judge.ToolOutcome{Status: judge.Verdict{Status: st}, Invoices: recs}
	}
	// check_company
	if st := fault("check_company", ""); st != 0 {
		tools["check_company"] = judge.ToolOutcome{Status: judge.Verdict{Status: st, Reason: "预置故障"}}
	} else {
		var recs []judge.CompanyRecord
		for _, co := range c.World.Companies {
			if co.TaxID == inv.SellerTaxID {
				recs = append(recs, judge.CompanyRecord{TaxID: co.TaxID, Name: co.Name, City: co.City, RegStatus: co.Status})
			}
		}
		st := judge.NotFound
		if len(recs) > 0 {
			st = judge.Found
		}
		tools["check_company"] = judge.ToolOutcome{Status: judge.Verdict{Status: st}, Companies: recs}
	}
	// check_history
	if st := fault("check_history", ""); st != 0 {
		tools["check_history"] = judge.ToolOutcome{Status: judge.Verdict{Status: st, Reason: "预置故障"}}
	} else {
		var recs []judge.HistoryRecord
		for _, h := range c.World.History {
			recs = append(recs, judge.HistoryRecord{ClaimNo: h.ClaimNo, InvoiceNo: h.InvoiceNo,
				ApplicantID: h.ApplicantID, SellerTaxID: h.SellerTaxID, IssueDate: h.IssueDate,
				AmountFen: h.AmountFen, Category: h.Category, RecStatus: h.Status, SubmitDate: h.SubmitDate})
		}
		tools["check_history"] = judge.ToolOutcome{Status: judge.Verdict{Status: judge.Found}, History: recs}
	}

	cl := c.Claim
	return &judge.Input{Claim: &cl, Fields: fields, Untrusted: untrusted, LowConfidence: low,
		SupportText: c.Support, Tools: tools}
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

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

var _ = strconv.Itoa
var _ = strings.TrimSpace
var _ = json.Marshal
