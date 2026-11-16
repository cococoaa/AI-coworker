package audit

import (
	"context"
	"time"

	"reimburse/internal/mcptools"
)

var _ Explorer = (*Mcp)(nil)

// Mcp 把 MCP 客户端适配成编排层的 Explorer。
//
// 为什么要这一层：internal/mcptools 实现了 MCP 协议与结果解析，但它不能导入 internal/audit
// （audit 的测试要导入 mcptools，会形成导入环）。所以两边的结果类型各自定义，
// 在这里做一次逐字段的显式转换——字段对不上会立刻编译失败，不会悄悄丢数据。
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
func (m *Mcp) VerifyInvoice(ctx context.Context, invoiceNo string) ToolResult {
	return conv(m.Client.VerifyInvoice(ctx, invoiceNo))
}

// CheckCompany 核验企业。
func (m *Mcp) CheckCompany(ctx context.Context, taxID string) ToolResult {
	return conv(m.Client.CheckCompany(ctx, taxID))
}

// CheckHistory 查询历史报销记录。
func (m *Mcp) CheckHistory(ctx context.Context, q HistoryQuery) ToolResult {
	return conv(m.Client.CheckHistory(ctx, mcptools.HistoryQuery{
		InvoiceNo: q.InvoiceNo, ApplicantID: q.ApplicantID, SellerTaxID: q.SellerTaxID,
	}))
}

// conv 把 mcptools.Result 转成本包的 ToolResult。
func conv(r mcptools.Result) ToolResult {
	return ToolResult{
		Status:    ToolStatus(r.Status),
		Detail:    r.Detail,
		Invoices:  r.Invoices,
		Companies: r.Companies,
		History:   r.History,
		Raw:       r.Raw,
	}
}

// ListTools 便于启动自检。
func (m *Mcp) ListTools(ctx context.Context) ([]string, error) { return m.Client.ListTools(ctx) }

// Timeout 设置单次工具调用上限。
func (m *Mcp) Timeout(d time.Duration) { m.Client.Timeout = d }
