package audit

import (
	"context"
	"sync"
	"time"
)

// RemoteSpy 包装任意 Explorer。它不改行为（委托给真实实现），只记录每次调用的原始返回与耗时。
//
// 用途：
//   - 把工具的真实返回放进事件流，评审能看到"工具到底回了什么"；
//   - 在任何实现之前就建立"调用必留痕"的约束，避免将来换实现时漏掉可观测性；
//   - 回放模式下复用同一条链路，录制与线上走同一段代码。
type RemoteSpy struct {
	Inner Explorer
	// Timeout 会作为 context 上限传给内层调用（如果内层支持）。
	Timeout time.Duration
	// Emit 在调用结束时调用，用于产生工具事件。
	Emit func(ev ToolEvent)
	// History 在内存里累计所有工具事件，便于测试与报告。
	mu      sync.Mutex
	History []ToolEvent
}

// ToolEvent 是一次工具调用的完整记录。
type ToolEvent struct {
	Tool     string        `json:"tool"`
	Args     string        `json:"args"`
	Status   Status        `json:"status"`
	Detail   string        `json:"detail,omitempty"`
	Records  int           `json:"records"`
	Raw      string        `json:"raw,omitempty"`
	Duration time.Duration `json:"duration_ns"`
	At       time.Time     `json:"at"`
}

var _ Explorer = (*RemoteSpy)(nil)

func (s *RemoteSpy) record(ev ToolEvent) {
	s.mu.Lock()
	s.History = append(s.History, ev)
	s.mu.Unlock()
	if s.Emit != nil {
		s.Emit(ev)
	}
}

// VerifyInvoice 调用内层并记录。
func (s *RemoteSpy) VerifyInvoice(ctx context.Context, invoiceNo string) ToolOutcome {
	t0 := time.Now()
	tr := s.Inner.VerifyInvoice(ctx, invoiceNo)
	s.record(ToolEvent{Tool: "verify_invoice", Args: "invoice_no=" + invoiceNo, Status: tr.Status.Status,
		Detail: tr.Status.Reason, Records: len(tr.Invoices), Raw: tr.Raw, Duration: time.Since(t0), At: t0})
	return tr
}

// CheckCompany 调用内层并记录。
func (s *RemoteSpy) CheckCompany(ctx context.Context, taxID string) ToolOutcome {
	t0 := time.Now()
	tr := s.Inner.CheckCompany(ctx, taxID)
	s.record(ToolEvent{Tool: "check_company", Args: "tax_id=" + taxID, Status: tr.Status.Status,
		Detail: tr.Status.Reason, Records: len(tr.Companies), Raw: tr.Raw, Duration: time.Since(t0), At: t0})
	return tr
}

// CheckHistory 调用内层并记录。
func (s *RemoteSpy) CheckHistory(ctx context.Context, q HistoryQuery) ToolOutcome {
	t0 := time.Now()
	tr := s.Inner.CheckHistory(ctx, q)
	s.record(ToolEvent{Tool: "check_history",
		Args:   "invoice_no=" + q.InvoiceNo + " applicant_id=" + q.ApplicantID + " seller_tax_id=" + q.SellerTaxID,
		Status: tr.Status.Status, Detail: tr.Status.Reason, Records: len(tr.History), Raw: tr.Raw, Duration: time.Since(t0), At: t0})
	return tr
}
