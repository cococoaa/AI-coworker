// Package schema 定义报销单、发票、佐证凭证、标准答案和 mock 工具侧数据的结构。
// 约定：金额一律为分（整数），日期一律为 YYYY-MM-DD。
package schema

// 发票在税务平台的登记状态。
const (
	InvNormal     = "normal"
	InvVoided     = "voided"
	InvRedReverse = "red_reversed"
)

// 销售方登记状态。
const (
	CoActive   = "存续"
	CoCanceled = "注销"
	CoRevoked  = "吊销"
	CoAbnormal = "经营异常"
)

// 历史报销记录状态。
const (
	HistApproved  = "approved"
	HistReviewing = "reviewing"
	HistRejected  = "rejected"
)

// 故障注入方式。
const (
	FaultTimeout = "timeout"
	FaultError   = "error"
	FaultGarbled = "garbled"
)

type Applicant struct {
	Name       string `json:"name"`
	EmployeeID string `json:"employee_id"`
	Grade      string `json:"grade"` // P 普通员工，M 经理，D 总监
}

type Attachments struct {
	InvoiceFile string `json:"invoice_file"`
	SupportFile string `json:"support_file"` // 空表示未上传佐证凭证
}

// Claim 是员工在报销系统里填写的报销单。
type Claim struct {
	ClaimNo             string      `json:"claim_no"`
	RequestNo           string      `json:"request_no"` // 出差或费用申请单号
	Applicant           Applicant   `json:"applicant"`
	Category            string      `json:"category"` // transport / lodging / meal
	FromCity            string      `json:"from_city,omitempty"`
	City                string      `json:"city"` // 目的地或费用发生地
	StartDate           string      `json:"start_date"`
	EndDate             string      `json:"end_date"`
	Purpose             string      `json:"purpose"`
	AmountFen           int64       `json:"amount_fen"`
	SubmitDate          string      `json:"submit_date"`
	LateExplanation     string      `json:"-"`
	InternalCount       int         `json:"internal_count,omitempty"`
	ExternalCount       int         `json:"external_count,omitempty"`
	ExternalOrg         string      `json:"external_org,omitempty"`
	EntertainApprovalNo string      `json:"entertain_approval_no,omitempty"`
	SpecialApprovalNo   string      `json:"special_approval_no,omitempty"`
	Attachments         Attachments `json:"attachments"`
}

// Invoice 是增值税电子发票上的结构化字段（标准答案）。
type Invoice struct {
	InvoiceNo   string `json:"invoice_no"`
	IssueDate   string `json:"issue_date"`
	BuyerName   string `json:"buyer_name"`
	BuyerTaxID  string `json:"buyer_tax_id"`
	SellerName  string `json:"seller_name"`
	SellerTaxID string `json:"seller_tax_id"`
	ItemName    string `json:"item_name"`
	TaxRatePct  int    `json:"tax_rate_pct"`
	ExTaxFen    int64  `json:"ex_tax_fen"`
	TaxFen      int64  `json:"tax_fen"`
	TotalFen    int64  `json:"total_fen"`
	Remark      string `json:"remark,omitempty"`
}

type Line struct {
	Date      string `json:"date,omitempty"`
	Name      string `json:"name"`
	Qty       int    `json:"qty,omitempty"`
	AmountFen int64  `json:"amount_fen"`
}

// Support 是行程单、住宿水单或消费清单上的结构化字段（标准答案）。
type Support struct {
	Kind     string `json:"kind"`             // itinerary / folio / consumption
	Mode     string `json:"mode,omitempty"`   // train / flight
	Person   string `json:"person,omitempty"` // 乘车人或入住人
	Date     string `json:"date,omitempty"`
	Vehicle  string `json:"vehicle,omitempty"` // 车次或航班号
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Seat     string `json:"seat,omitempty"`
	Hotel    string `json:"hotel,omitempty"`
	City     string `json:"city,omitempty"`
	CheckIn  string `json:"check_in,omitempty"`
	CheckOut string `json:"check_out,omitempty"`
	Nights   int    `json:"nights,omitempty"`
	Venue    string `json:"venue,omitempty"`
	Lines    []Line `json:"lines,omitempty"`
	TotalFen int64  `json:"total_fen"`
}

// Truth 是一条测试用例的标准答案。
type Truth struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Tags        []string `json:"tags"`
	Notes       string   `json:"notes,omitempty"`
	Decision    string   `json:"decision"`
	Hits        []string `json:"hits"`
	ApprovedFen int64    `json:"approved_fen"` // 仅 approve 与 approve_with_adjustment 有意义
	Invoice     Invoice  `json:"invoice"`
	Support     *Support `json:"support,omitempty"`
	Unreadable  []string `json:"unreadable,omitempty"` // 故意弄得无法辨认的字段
}

// RegInvoice 是税务平台登记的发票（verify_invoice 的后端数据）。
type RegInvoice struct {
	InvoiceNo   string `json:"invoice_no"`
	IssueDate   string `json:"issue_date"`
	TotalFen    int64  `json:"total_fen"`
	BuyerTaxID  string `json:"buyer_tax_id"`
	SellerTaxID string `json:"seller_tax_id"`
	Status      string `json:"status"`
}

// Company 是企业登记信息（check_company 的后端数据）。
type Company struct {
	TaxID  string `json:"tax_id"`
	Name   string `json:"name"`
	City   string `json:"city"`
	Status string `json:"status"`
}

// HistRecord 是历史报销记录（check_history 的后端数据）。
type HistRecord struct {
	ClaimNo       string `json:"claim_no"`
	InvoiceNo     string `json:"invoice_no"`
	ApplicantID   string `json:"applicant_id"`
	SellerTaxID   string `json:"seller_tax_id"`
	IssueDate     string `json:"issue_date"`
	AmountFen     int64  `json:"amount_fen"`
	Category      string `json:"category"`
	TransportMode string `json:"transport_mode,omitempty"`
	Status        string `json:"status"`
	SubmitDate    string `json:"submit_date"`
}

// Fault 描述一次预置的工具故障。Key 对 verify_invoice 和 check_history 是发票号码，对 check_company 是统一社会信用代码。
type Fault struct {
	Tool string `json:"tool"`
	Key  string `json:"key"`
	Mode string `json:"mode"`
}

// World 是一条用例对应的 mock 工具后端数据。
type World struct {
	Invoices  []RegInvoice `json:"invoices"`
	Companies []Company    `json:"companies"`
	History   []HistRecord `json:"history"`
	Faults    []Fault      `json:"faults"`
}

// Case 是一条完整的测试用例。
type Case struct {
	ID          string
	Claim       Claim
	InvoiceText string
	SupportText string // 空表示缺失
	Truth       Truth
	World       World
	// HasTruth 表示目录里带了 truth.json（标准答案）。
	// 真实票据没有标准答案：Truth 会是零值，绝不能把零值当成"标准答案是空的"来用。
	HasTruth bool
}
