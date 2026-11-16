package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"reimburse/internal/schema"
)

// Remote 是 MCP 客户端，实现 Explorer，编排层只知道这个接口。
//
// 它做三件事：
//  1. 把业务调用翻译成 MCP 工具调用；
//  2. 把 MCP 的返回（结构化内容 / 纯文本 / isError / 协议错误 / 超时）翻译成 ToolResult；
//  3. 每次调用都记录耗时与原始返回，供界面展示"工具到底返回了什么"。
//
// 关键约定：解析不出合法结构时一律判为 failed，绝不退化成"空结果"，
// 因为空结果会被规则层理解成"查无此票"进而误驳回。
type Remote struct {
	Session *mcp.ClientSession
	// Timeout 是单次工具调用上限，超时判为 timeout（状态未知）。
	Timeout time.Duration
	// OnCall 在每次调用结束后回调，用于事件流。
	OnCall func(tool string, args any, res Result, raw string, d time.Duration)
}

var _ Explorer = (*Remote)(nil)

// Client 建立连接。
func Client(ctx context.Context, tr mcp.Transport, impl *mcp.Implementation) (*mcp.ClientSession, error) {
	if impl == nil {
		impl = &mcp.Implementation{Name: "reimburse-audit", Version: "1.0.0"}
	}
	c := mcp.NewClient(impl, nil)
	return c.Connect(ctx, tr, nil)
}

// ListTools 列出服务端提供的工具，用于启动自检：确认三个工具都在。
func (r *Remote) ListTools(ctx context.Context) ([]string, error) {
	res, err := r.Session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, t := range res.Tools {
		names = append(names, t.Name)
	}
	return names, nil
}

func (r *Remote) call(ctx context.Context, tool string, args any, out any) Result {
	t0 := time.Now()
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	b, _ := json.Marshal(args)
	res, err := r.Session.CallTool(cctx, &mcp.CallToolParams{Name: tool, Arguments: json.RawMessage(b)})
	d := time.Since(t0)
	raw := ""
	if res != nil {
		raw = textOf(res)
	}
	tr := classify(res, err, raw, out)
	tr.Raw = raw
	if r.OnCall != nil {
		r.OnCall(tool, args, tr, raw, d)
	}
	return tr
}

// classify 把一次调用归类。
func classify(res *mcp.CallToolResult, err error, raw string, out any) Result {
	if err != nil {
		if isTimeout(err) {
			return Result{Status: ResultTimeout, Detail: "调用超时：" + err.Error()}
		}
		return Result{Status: ResultFailed, Detail: "调用失败：" + err.Error()}
	}
	if res == nil {
		return Result{Status: ResultFailed, Detail: "服务端返回空"}
	}
	// 优先用结构化内容；没有则解析文本内容。
	var payload []byte
	switch v := res.StructuredContent.(type) {
	case nil:
		payload = []byte(raw)
	case json.RawMessage:
		payload = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return Result{Status: ResultFailed, Detail: "结构化内容无法序列化"}
		}
		payload = b
	}
	if err := json.Unmarshal(payload, out); err != nil {
		// 乱码、截断、格式不符都到这里。绝不能当成空结果。
		return Result{Status: ResultFailed,
			Detail: "返回内容不是合法 JSON：" + truncate(sanitize(raw), 120)}
	}
	status := fieldStatus(payload)
	switch {
	case res.IsError && status == StatusTimeout:
		return Result{Status: ResultTimeout, Detail: noteOf(payload, "工具报告超时")}
	case res.IsError || status == StatusFailed:
		// 业务参数缺失时服务端按协议错误返回，此时没有结构化内容，原因在文本里。
		note := noteOf(payload, "")
		if note == "" {
			note = "工具报告失败：" + truncate(sanitize(raw), 160)
		}
		return Result{Status: ResultFailed, Detail: note}
	case status == StatusTimeout:
		return Result{Status: ResultTimeout, Detail: noteOf(payload, "工具报告超时")}
	}
	return Result{Status: ResultOK}
}

// fieldStatus 取返回里的 status 字段（ok / failed / timeout）。
func fieldStatus(b []byte) string {
	var m struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	return m.Status
}

func noteOf(b []byte, def string) string {
	var m struct {
		Note string `json:"note"`
	}
	if json.Unmarshal(b, &m) == nil && m.Note != "" {
		return m.Note
	}
	return def
}

func textOf(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if err == context.DeadlineExceeded {
		return true
	}
	return strings.Contains(err.Error(), "context deadline exceeded")
}

func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

// sanitize 去掉控制字符，避免把乱码原样写进日志时破坏排版。
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)
}

// ---- 三个工具的适配 ----

// VerifyInvoice 调用 verify_invoice。
func (r *Remote) VerifyInvoice(ctx context.Context, invoiceNo string) Result {
	var out OutInvoice
	tr := r.call(ctx, ToolVerifyInvoice, InInvoice{InvoiceNo: invoiceNo}, &out)
	if tr.Status != ResultOK {
		return tr
	}
	tr.Invoices = regInvoices(out)
	return tr
}

// CheckCompany 调用 check_company。
func (r *Remote) CheckCompany(ctx context.Context, taxID string) Result {
	var out OutCompany
	tr := r.call(ctx, ToolCheckCompany, InCompany{TaxID: taxID}, &out)
	if tr.Status != ResultOK {
		return tr
	}
	tr.Companies = companies(out)
	return tr
}

// CheckHistory 调用 check_history。
func (r *Remote) CheckHistory(ctx context.Context, q HistoryQuery) Result {
	var out OutHistory
	tr := r.call(ctx, ToolCheckHistory, InHistory{
		InvoiceNo: q.InvoiceNo, ApplicantID: q.ApplicantID, SellerTaxID: q.SellerTaxID,
	}, &out)
	if tr.Status != ResultOK {
		return tr
	}
	if out.Truncated {
		tr.Detail = out.Note
	}
	tr.History = histRecords(out)
	return tr
}

// regInvoices 把元字符串转回分。转换失败说明数据有问题，直接报错而不是补 0。
func regInvoices(o OutInvoice) []schema.RegInvoice {
	var out []schema.RegInvoice
	for _, i := range o.Invoices {
		fen, err := ParseYuan(i.TotalYuan)
		if err != nil {
			return nil
		}
		out = append(out, schema.RegInvoice{
			InvoiceNo: i.InvoiceNo, IssueDate: i.IssueDate, TotalFen: fen,
			BuyerTaxID: i.BuyerTaxID, SellerTaxID: i.SellerTaxID, Status: i.Status,
		})
	}
	return out
}

func companies(o OutCompany) []schema.Company {
	var out []schema.Company
	for _, c := range o.Companies {
		out = append(out, schema.Company{TaxID: c.TaxID, Name: c.Name, City: c.City, Status: c.Status})
	}
	return out
}

func histRecords(o OutHistory) []schema.HistRecord {
	var out []schema.HistRecord
	for _, h := range o.Records {
		fen, _ := ParseYuan(h.AmountYuan)
		out = append(out, schema.HistRecord{
			ClaimNo: h.ClaimNo, InvoiceNo: h.InvoiceNo, ApplicantID: h.ApplicantID,
			SellerTaxID: h.SellerTaxID, IssueDate: h.IssueDate, AmountFen: fen,
			Category: h.Category, Status: h.Status, SubmitDate: h.SubmitDate,
		})
	}
	return out
}

// parseYuan 解析两位小数的元字符串。
func ParseYuan(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("金额为空")
	}
	var whole, frac int64
	neg := false
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	parts := strings.SplitN(s, ".", 2)
	if _, err := fmt.Sscanf(parts[0], "%d", &whole); err != nil {
		return 0, fmt.Errorf("金额格式错误: %q", s)
	}
	if len(parts) == 2 {
		f := parts[1]
		if len(f) == 1 {
			f += "0"
		}
		if len(f) > 2 {
			f = f[:2]
		}
		if _, err := fmt.Sscanf(f, "%d", &frac); err != nil {
			return 0, fmt.Errorf("金额格式错误: %q", s)
		}
	}
	v := whole*100 + frac
	if neg {
		v = -v
	}
	return v, nil
}
