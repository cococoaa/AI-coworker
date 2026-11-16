// Package mcptools 把三个核验能力做成进程外 MCP 工具：verify_invoice、check_company、check_history。
//
// 三条设计约束：
//  1. 只读。三个工具没有任何写操作，无法改凭证、改报销单或冲销发票。
//  2. 只回相关记录。按发票号、统一社会信用代码、申请人等最少条件查询，不整库返回，
//     避免把与本次审核无关的历史数据带进模型上下文。
//  3. 故障显式化。超时、后端报错、返回乱码都以 isError 或结构化状态返回，
//     由上层判为「未核验」转人工，绝不以空结果冒充实查。
package mcptools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"reimburse/internal/schema"
)

// 工具名。与规则表 params.tool_deps 的键一一对应。
const (
	ToolVerifyInvoice = "verify_invoice"
	ToolCheckCompany  = "check_company"
	ToolCheckHistory  = "check_history"
)

// 工具返回状态。与 audit.ToolStatus 取值一致，便于直接映射。
const (
	StatusOK      = "ok"
	StatusFailed  = "failed"
	StatusTimeout = "timeout"
)

// InInvoice 是 verify_invoice 的入参。
type InInvoice struct {
	InvoiceNo string `json:"invoice_no" jsonschema:"待查验的增值税电子发票号码，18 位数字"`
}

// InCompany 是 check_company 的入参。
type InCompany struct {
	TaxID string `json:"tax_id" jsonschema:"销售方的统一社会信用代码，18 位，允许数字与大写字母"`
}

// InHistory 是 check_history 的入参。最多按三个条件组合查询，传空表示该条件不参与。
type InHistory struct {
	InvoiceNo   string `json:"invoice_no,omitempty" jsonschema:"发票号码；用于判断同一张发票是否已经报过"`
	ApplicantID string `json:"applicant_id,omitempty" jsonschema:"申请人员工编号；用于取同一申请人的历史记录，检查拆单和换票重复报销"`
	SellerTaxID string `json:"seller_tax_id,omitempty" jsonschema:"销售方统一社会信用代码；用于判断是否在同一商户连续报销"`
}

// InvoiceInfo 是发票在税务平台的登记信息。
type InvoiceInfo struct {
	InvoiceNo   string `json:"invoice_no"`
	IssueDate   string `json:"issue_date"`
	TotalYuan   string `json:"total_yuan"` // 元，两位小数；分只用于内部计算，避免模型换算出错
	BuyerTaxID  string `json:"buyer_tax_id"`
	SellerTaxID string `json:"seller_tax_id"`
	Status      string `json:"status"` // normal / voided / red_reversed
	StatusText  string `json:"status_text"`
}

// CompanyInfo 是企业登记信息。
type CompanyInfo struct {
	TaxID      string `json:"tax_id"`
	Name       string `json:"name"`
	City       string `json:"city"`
	Status     string `json:"status"`
	StatusText string `json:"status_text"`
}

// HistoryInfo 是一条历史报销记录。
type HistoryInfo struct {
	ClaimNo     string `json:"claim_no"`
	InvoiceNo   string `json:"invoice_no"`
	ApplicantID string `json:"applicant_id"`
	SellerTaxID string `json:"seller_tax_id"`
	IssueDate   string `json:"issue_date"`
	AmountYuan  string `json:"amount_yuan"`
	Category    string `json:"category"`
	Status      string `json:"status"`
	StatusText  string `json:"status_text"`
	SubmitDate  string `json:"submit_date"`
}

// OutInvoice 是 verify_invoice 的返回。
type OutInvoice struct {
	Status    string        `json:"status"`   // ok / failed / timeout
	Found     bool          `json:"found"`    // 税务平台是否登记了该发票
	Invoices  []InvoiceInfo `json:"invoices"` // 通常 0 或 1 条
	Note      string        `json:"note,omitempty"`
	LatencyMS int64         `json:"latency_ms"`
}

// OutCompany 是 check_company 的返回。
type OutCompany struct {
	Status    string        `json:"status"`
	Found     bool          `json:"found"`
	Companies []CompanyInfo `json:"companies"`
	Note      string        `json:"note,omitempty"`
	LatencyMS int64         `json:"latency_ms"`
}

// OutHistory 是 check_history 的返回。
type OutHistory struct {
	Status    string        `json:"status"`
	Records   []HistoryInfo `json:"records"`
	Truncated bool          `json:"truncated,omitempty"` // 命中过多被截断
	Note      string        `json:"note,omitempty"`
	LatencyMS int64         `json:"latency_ms"`
}

// 单次返回上限。历史记录超过上限时截断并置 Truncated，避免一次调用撑爆上下文与带宽。
const historyLimit = 200

// DataSource 是三个工具背后的数据来源（本地 mock 库或真实登记系统）。
type DataSource interface {
	LookupInvoice(no string) ([]schema.RegInvoice, error)
	LookupCompany(taxID string) ([]schema.Company, error)
	LookupHistory(q HistoryKey) ([]schema.HistRecord, error)
}

// HistoryKey 是查重条件。
type HistoryKey struct {
	InvoiceNo   string
	ApplicantID string
	SellerTaxID string
}

// Store 实现 DataSource，数据来自 JSON（world.json 或现场演示库）。
// 索引在加载时建立一次，查询是 O(1) 或 O(k)。
type Store struct {
	invoices        map[string][]schema.RegInvoice
	byBuyer         map[string][]schema.RegInvoice
	companies       map[string][]schema.Company
	histByNo        map[string][]schema.HistRecord
	histByApplicant map[string][]schema.HistRecord
	histBySeller    map[string][]schema.HistRecord
}

// NewStore 从 World 建立内存索引。
func NewStore(w schema.World) *Store {
	s := &Store{
		invoices: map[string][]schema.RegInvoice{}, byBuyer: map[string][]schema.RegInvoice{},
		companies: map[string][]schema.Company{},
		histByNo:  map[string][]schema.HistRecord{}, histByApplicant: map[string][]schema.HistRecord{},
		histBySeller: map[string][]schema.HistRecord{},
	}
	for _, r := range w.Invoices {
		s.invoices[r.InvoiceNo] = append(s.invoices[r.InvoiceNo], r)
		s.byBuyer[r.BuyerTaxID] = append(s.byBuyer[r.BuyerTaxID], r)
	}
	for _, c := range w.Companies {
		s.companies[c.TaxID] = append(s.companies[c.TaxID], c)
	}
	for _, h := range w.History {
		s.histByNo[h.InvoiceNo] = append(s.histByNo[h.InvoiceNo], h)
		s.histByApplicant[h.ApplicantID] = append(s.histByApplicant[h.ApplicantID], h)
		s.histBySeller[h.SellerTaxID] = append(s.histBySeller[h.SellerTaxID], h)
	}
	return s
}

func (s *Store) LookupInvoice(no string) ([]schema.RegInvoice, error) {
	return s.invoices[no], nil
}

func (s *Store) LookupCompany(taxID string) ([]schema.Company, error) {
	return s.companies[taxID], nil
}

// LookupHistory 返回满足全部非空条件的记录，按发票日期倒序。
// 条件之间是"与"，但发票号与申请人之间取"或"：查重既可能因为同一张票，也可能因为同一人的可疑记录。
func (s *Store) LookupHistory(q HistoryKey) ([]schema.HistRecord, error) {
	if q.InvoiceNo == "" && q.ApplicantID == "" && q.SellerTaxID == "" {
		return nil, fmt.Errorf("至少需要一个查询条件")
	}
	seen := map[string]bool{}
	var out []schema.HistRecord
	add := func(rs []schema.HistRecord) {
		for _, r := range rs {
			k := r.ClaimNo + "|" + r.InvoiceNo
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, r)
		}
	}
	if q.InvoiceNo != "" {
		add(s.histByNo[q.InvoiceNo])
	}
	if q.ApplicantID != "" {
		add(filter(s.histByApplicant[q.ApplicantID], q))
	}
	if q.SellerTaxID != "" && q.ApplicantID == "" && q.InvoiceNo == "" {
		add(filter(s.histBySeller[q.SellerTaxID], q))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IssueDate != out[j].IssueDate {
			return out[i].IssueDate > out[j].IssueDate
		}
		return out[i].ClaimNo < out[j].ClaimNo
	})
	return out, nil
}

func filter(rs []schema.HistRecord, q HistoryKey) []schema.HistRecord {
	var out []schema.HistRecord
	for _, r := range rs {
		if q.SellerTaxID != "" && r.SellerTaxID != q.SellerTaxID {
			continue
		}
		out = append(out, r)
	}
	return out
}

// Yuan 把分转成两位小数的元字符串，避免模型做"分到元"的换算。
func Yuan(fen int64) string { return fmt.Sprintf("%d.%02d", fen/100, fen%100) }

func invoiceStatusText(s string) string {
	switch s {
	case schema.InvNormal:
		return "正常"
	case schema.InvVoided:
		return "已作废"
	case schema.InvRedReverse:
		return "已红字冲销"
	}
	return s
}

func companyStatusText(s string) string {
	switch s {
	case schema.CoActive:
		return "存续"
	case schema.CoCanceled:
		return "注销"
	case schema.CoRevoked:
		return "吊销"
	case schema.CoAbnormal:
		return "经营异常"
	}
	return s
}

func histStatusText(s string) string {
	switch s {
	case schema.HistApproved:
		return "已通过"
	case schema.HistReviewing:
		return "审核中"
	case schema.HistRejected:
		return "已驳回"
	}
	return s
}

// ---- 业务实现 ----

// Verify 查询发票登记信息。查无此票返回 Found=false、Status=ok：
// "没查到"是确定的事实，不是工具故障，两者必须区分。
func Verify(ds DataSource, in InInvoice, now func() time.Time) (OutInvoice, error) {
	t0 := now()
	no := strings.TrimSpace(in.InvoiceNo)
	if no == "" {
		return OutInvoice{}, fmt.Errorf("invoice_no 不能为空")
	}
	rs, err := ds.LookupInvoice(no)
	if err != nil {
		return OutInvoice{Status: StatusFailed, Note: "税务平台查询失败: " + err.Error(),
			LatencyMS: now().Sub(t0).Milliseconds()}, nil
	}
	out := OutInvoice{Status: StatusOK, LatencyMS: now().Sub(t0).Milliseconds()}
	for _, r := range rs {
		out.Invoices = append(out.Invoices, InvoiceInfo{
			InvoiceNo: r.InvoiceNo, IssueDate: r.IssueDate, TotalYuan: Yuan(r.TotalFen),
			BuyerTaxID: r.BuyerTaxID, SellerTaxID: r.SellerTaxID,
			Status: r.Status, StatusText: invoiceStatusText(r.Status),
		})
	}
	out.Found = len(out.Invoices) > 0
	if !out.Found {
		out.Note = "税务平台未登记该发票号码"
	}
	return out, nil
}

// CheckCompany 查询企业登记状态。
func CheckCompany(ds DataSource, in InCompany, now func() time.Time) (OutCompany, error) {
	t0 := now()
	id := strings.ToUpper(strings.TrimSpace(in.TaxID))
	if id == "" {
		return OutCompany{}, fmt.Errorf("tax_id 不能为空")
	}
	cs, err := ds.LookupCompany(id)
	if err != nil {
		return OutCompany{Status: StatusFailed, Note: "企业登记系统查询失败: " + err.Error(),
			LatencyMS: now().Sub(t0).Milliseconds()}, nil
	}
	out := OutCompany{Status: StatusOK, LatencyMS: now().Sub(t0).Milliseconds()}
	for _, c := range cs {
		out.Companies = append(out.Companies, CompanyInfo{
			TaxID: c.TaxID, Name: c.Name, City: c.City,
			Status: c.Status, StatusText: companyStatusText(c.Status),
		})
	}
	out.Found = len(out.Companies) > 0
	if !out.Found {
		out.Note = "企业登记系统未查到该统一社会信用代码"
	}
	return out, nil
}

// CheckHistory 查询历史报销记录，用于查重与拆单。
func CheckHistory(ds DataSource, in InHistory, now func() time.Time) (OutHistory, error) {
	t0 := now()
	q := HistoryKey{InvoiceNo: strings.TrimSpace(in.InvoiceNo),
		ApplicantID: strings.TrimSpace(in.ApplicantID), SellerTaxID: strings.ToUpper(strings.TrimSpace(in.SellerTaxID))}
	if q.InvoiceNo == "" && q.ApplicantID == "" && q.SellerTaxID == "" {
		return OutHistory{}, fmt.Errorf("invoice_no、applicant_id、seller_tax_id 至少提供一个")
	}
	rs, err := ds.LookupHistory(q)
	if err != nil {
		return OutHistory{Status: StatusFailed, Note: "历史库查询失败: " + err.Error(),
			LatencyMS: now().Sub(t0).Milliseconds()}, nil
	}
	out := OutHistory{Status: StatusOK, LatencyMS: now().Sub(t0).Milliseconds()}
	if len(rs) > historyLimit {
		rs = rs[:historyLimit]
		out.Truncated = true
		out.Note = fmt.Sprintf("命中超过 %d 条，已截断；请缩小查询范围", historyLimit)
	}
	for _, r := range rs {
		out.Records = append(out.Records, HistoryInfo{
			ClaimNo: r.ClaimNo, InvoiceNo: r.InvoiceNo, ApplicantID: r.ApplicantID,
			SellerTaxID: r.SellerTaxID, IssueDate: r.IssueDate, AmountYuan: Yuan(r.AmountFen),
			Category: r.Category, Status: r.Status, StatusText: histStatusText(r.Status),
			SubmitDate: r.SubmitDate,
		})
	}
	return out, nil
}

// Marshal 便于测试与日志。
func Marshal(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
