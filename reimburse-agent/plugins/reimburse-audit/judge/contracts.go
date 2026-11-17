package judge

import (
	"context"
	"errors"
)

// Status 是外部事实的核验结果。四态语义各不相同，不能混用：
//
//	Found     查到      可据此判定
//	NotFound  查无      可据此判定（"查无此票"本身是驳回依据）
//	Failed    失败      不可据此判定
//	Timeout   超时      不可据此判定——既非查到也非查无
//
// 用 uint8 而不是字符串，是为了让 Usable() 成为唯一出口：
// 判定代码只能通过 Status.Usable() 判断能不能用，不能自己写 == "timeout" 这种比较。
type Status uint8

const (
	// Found 查到记录。
	Found Status = iota + 1
	// NotFound 确定查无。这是事实，不是故障。
	NotFound
	// Failed 查询失败（接口报错、返回无法解析）。
	Failed
	// Timeout 超时。状态未知。
	Timeout
)

func (s Status) String() string {
	switch s {
	case Found:
		return "found"
	case NotFound:
		return "not_found"
	case Failed:
		return "failed"
	case Timeout:
		return "timeout"
	}
	return "unknown"
}

// Usable 表示该结果能否作为判定依据。
// 只有 Found 与 NotFound 可用；Failed 与 Timeout 必须让相关判定项"不作判定"。
func (s Status) Usable() bool { return s == Found || s == NotFound }

// Skipped 表示这次核验压根没发起（前置条件缺失，例如发票号读不出来）。
// 它与 Failed 的区别：Failed 是"问了但没答上来"，Skipped 是"没问"。
// 两者对判定的效果相同（不作判定），但给人工复核的信息不同。
func (s Status) OrSkipped() Status {
	if s == 0 {
		return Failed
	}
	return s
}

// Verdict 是一次核验的结果。Reason 是给人看的一句话，不含敏感数据。
type Verdict struct {
	Status Status
	Reason string
	// Records 是查到的原始记录条数（仅用于展示"查到了几条"）。
	Records int
}

// InvoiceRecord 是发票在税务平台的登记信息。
// 金额一律以"分"为单位，避免字符串解析；日期为 YYYY-MM-DD。
type InvoiceRecord struct {
	InvoiceNo   string
	IssueDate   string
	TotalFen    int64
	BuyerTaxID  string
	SellerTaxID string
	// PlatformStatus 是平台侧状态：normal / voided / red_reversed。
	PlatformStatus string
}

// CompanyRecord 是企业登记信息。
type CompanyRecord struct {
	TaxID string
	Name  string
	City  string
	// RegStatus 是登记状态：存续 / 注销 / 吊销 / 经营异常。
	RegStatus string
}

// HistoryRecord 是一条历史报销记录。
type HistoryRecord struct {
	ClaimNo     string
	InvoiceNo   string
	ApplicantID string
	SellerTaxID string
	IssueDate   string
	AmountFen   int64
	Category    string
	// RecStatus 是历史单据状态：approved / reviewing / rejected。
	RecStatus  string
	SubmitDate string
}

// HistoryQuery 是查重条件。至少给一个，否则宿主应返回错误。
// 按规则 R04/R05 所需的最少字段传，不做全表扫描。
type HistoryQuery struct {
	InvoiceNo   string
	ApplicantID string
	SellerTaxID string
	// IssueDate 用于"同人同销售方同日同额、发票号不同"的换票重报识别（R04-b）。
	IssueDate string
}

// InvoiceVerifier 查验发票是否真实存在、是否作废或红冲。
type InvoiceVerifier interface {
	// Verify 返回该发票号的登记信息。
	// 平台查无此票时返回 Status=NotFound 且 Records 为空，不是错误。
	Verify(ctx context.Context, invoiceNo string) (Verdict, []InvoiceRecord)
}

// CompanyVerifier 查询销售方企业登记状态。
type CompanyVerifier interface {
	Check(ctx context.Context, taxID string) (Verdict, []CompanyRecord)
}

// HistoryFinder 查询历史报销记录，用于查重与拆单识别。
type HistoryFinder interface {
	Find(ctx context.Context, q HistoryQuery) (Verdict, []HistoryRecord)
}

// Tools 把三个核验能力打包，宿主只需构造一次。
type Tools struct {
	Invoice InvoiceVerifier
	Company CompanyVerifier
	History HistoryFinder
}

// ErrNoTools 表示宿主没有提供工具。此时所有依赖工具的判定项都应转人工。
var ErrNoTools = errors.New("judge: 未注入核验工具")
