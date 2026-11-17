package audit

import (
	"context"

	"reimburse/internal/mcptools"
	"reimburse/internal/schema"
	"reimburse/plugins/reimburse-audit/judge"
)

// Mcp 把 MCP 客户端适配成编排层的 Explorer。
//
// 它做的是翻译：MCP 客户端的 mcptools.Result（字符串状态 + schema 记录）
// 翻译成判定插件要求的 judge.ToolOutcome（四态 Status + 插件自有记录类型）。
//
// 翻译必须逐字段显式写，不能用反射或 JSON 往返——字段增删时编译器会立刻报错，
// 不会出现"新增字段悄悄丢失"。
type Mcp struct {
	Client *mcptools.Remote
}

var _ Explorer = (*Mcp)(nil)

// WrapMCP 适配 MCP 客户端。client 为空返回 nil。
func WrapMCP(client *mcptools.Remote) Explorer {
	if client == nil {
		return nil
	}
	return &Mcp{Client: client}
}

// VerifyInvoice 查验发票。
func (m *Mcp) VerifyInvoice(ctx context.Context, invoiceNo string) ToolOutcome {
	return conv(m.Client.VerifyInvoice(ctx, invoiceNo))
}

// CheckCompany 核验企业。
func (m *Mcp) CheckCompany(ctx context.Context, taxID string) ToolOutcome {
	return conv(m.Client.CheckCompany(ctx, taxID))
}

// CheckHistory 查询历史报销记录。
func (m *Mcp) CheckHistory(ctx context.Context, q HistoryQuery) ToolOutcome {
	return conv(m.Client.CheckHistory(ctx, mcptools.HistoryQuery{
		InvoiceNo: q.InvoiceNo, ApplicantID: q.ApplicantID, SellerTaxID: q.SellerTaxID,
	}))
}

// ListTools 便于启动自检。
func (m *Mcp) ListTools(ctx context.Context) ([]string, error) { return m.Client.ListTools(ctx) }

// conv 把 mcptools.Result 翻译成 judge.ToolOutcome。
//
// 状态映射是关键：mcptools 的 "ok" → Found，"failed" → Failed，"timeout" → Timeout。
// 注意：mcptools 的 ok 只表示"调用成功"，到底查到没查到由记录是否为空决定，
// 所以这里要根据记录数把 ok 再细分成 Found / NotFound。
func conv(r mcptools.Result) judge.ToolOutcome {
	out := judge.ToolOutcome{Raw: r.Raw}
	switch r.Status {
	case mcptools.ResultOK:
		// 由记录数决定 Found / NotFound。
		n := len(r.Invoices) + len(r.Companies) + len(r.History)
		if n > 0 {
			out.Status = judge.Verdict{Status: judge.Found, Records: n}
		} else {
			out.Status = judge.Verdict{Status: judge.NotFound}
		}
	case mcptools.ResultTimeout:
		out.Status = judge.Verdict{Status: judge.Timeout, Reason: r.Detail}
	case mcptools.ResultSkipped:
		out.Status = judge.Verdict{Status: judge.Failed, Reason: r.Detail}
	default:
		out.Status = judge.Verdict{Status: judge.Failed, Reason: r.Detail}
	}

	for _, i := range r.Invoices {
		out.Invoices = append(out.Invoices, judge.InvoiceRecord{
			InvoiceNo: i.InvoiceNo, IssueDate: i.IssueDate, TotalFen: i.TotalFen,
			BuyerTaxID: i.BuyerTaxID, SellerTaxID: i.SellerTaxID, PlatformStatus: i.Status,
		})
	}
	for _, c := range r.Companies {
		out.Companies = append(out.Companies, judge.CompanyRecord{
			TaxID: c.TaxID, Name: c.Name, City: c.City, RegStatus: c.Status,
		})
	}
	for _, h := range r.History {
		out.History = append(out.History, judge.HistoryRecord{
			ClaimNo: h.ClaimNo, InvoiceNo: h.InvoiceNo, ApplicantID: h.ApplicantID,
			SellerTaxID: h.SellerTaxID, IssueDate: h.IssueDate, AmountFen: h.AmountFen,
			Category: h.Category, RecStatus: h.Status, SubmitDate: h.SubmitDate,
		})
	}
	return out
}

var _ = schema.Claim{}
