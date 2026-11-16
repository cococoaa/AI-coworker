package mcptools

// 本包不导入 internal/audit（避免与测试形成导入环），
// 这里定义与 audit.ToolResult 字段完全一致的结果类型，
// 由 Unwrap 做一次显式转换。转换是逐字段的，字段增减会立刻编译失败。

import (
	"context"

	"reimburse/internal/schema"
)

// ResultStatus 与 audit.ToolStatus 取值一致。
type ResultStatus string

const (
	ResultOK      ResultStatus = "ok"
	ResultFailed  ResultStatus = "failed"
	ResultTimeout ResultStatus = "timeout"
	ResultSkipped ResultStatus = "skipped"
)

// Result 是一次工具调用的结果。
type Result struct {
	Status    ResultStatus        `json:"status"`
	Detail    string              `json:"detail,omitempty"`
	Invoices  []schema.RegInvoice `json:"invoices,omitempty"`
	Companies []schema.Company    `json:"companies,omitempty"`
	History   []schema.HistRecord `json:"history,omitempty"`
	Raw       string              `json:"raw,omitempty"`
}

// HistoryQuery 是查重条件，字段与 audit.HistoryQuery 一致。
type HistoryQuery struct {
	InvoiceNo   string
	ApplicantID string
	SellerTaxID string
}

// Explorer 与 audit.Explorer 方法集一致。
type Explorer interface {
	VerifyInvoice(ctx context.Context, invoiceNo string) Result
	CheckCompany(ctx context.Context, taxID string) Result
	CheckHistory(ctx context.Context, q HistoryQuery) Result
}
