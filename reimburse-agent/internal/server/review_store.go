package server

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// 复核结论的存储与撤销。
//
// 为什么需要撤销：复核结论会写进查重库（extraHistory），从而影响后续单据的
// 重复报销判定。如果复核人点错了"确认通过"，那张发票以后就会被判"已报销过"——
// 这是一个会悄悄扩散的副作用。只追加不撤销的日志，等于错了没法改。
//
// 撤销语义：**软删除**。记录不删除，只标记 RevokedAt / RevokedBy / RevokeReason，
// 并从查重库里把对应历史记录撤掉。这样审计能看到"谁批的、谁撤的、为什么撤"，
// 而不是凭空少了一条。
//
// 并发：所有读写走同一把锁；extraHistory 由 reviewStore 单独维护，
// Server.extra() 返回快照，避免调用方直接改内部切片。

// ReviewOutcome 是一条人工复核结论。
type ReviewOutcome struct {
	// ID 是结论编号，撤销时用它定位。
	ID string `json:"id"`
	// At 是提交时间。
	At       time.Time `json:"at"`
	CaseID   string    `json:"case_id"`
	ClaimNo  string    `json:"claim_no"`
	Decision string    `json:"decision"`
	Note     string    `json:"note"`
	// Operator 是提交人工号；OperatorName 是姓名（展示用）。
	Operator     string `json:"operator"`
	OperatorName string `json:"operator_name,omitempty"`
	// DecisionBefore 是提交时系统的自动判定，便于对比"人是否改了结论"。
	DecisionBefore string `json:"decision_before,omitempty"`
	// AffectedHistory 记录这条结论往查重库里写了什么，撤销时按此回滚。
	AffectedHistory HistoryRef `json:"affected_history,omitempty"`
	// RevokedAt 非空表示已撤销。
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	RevokedBy     string     `json:"revoked_by,omitempty"`
	RevokedByName string     `json:"revoked_by_name,omitempty"`
	RevokeReason  string     `json:"revoke_reason,omitempty"`
}

// Revoked 报告是否已撤销。
func (r ReviewOutcome) Revoked() bool { return r.RevokedAt != nil }

// HistoryRef 描述一条复核结论对查重库的影响，用于回滚。
type HistoryRef struct {
	// ClaimNo 是写入 extraHistory 的记录主键（带 -RV 后缀）。
	ClaimNo string `json:"claim_no,omitempty"`
	// InvoiceNo 是受影响的发票号。
	InvoiceNo string `json:"invoice_no,omitempty"`
	// Applied 为真表示确实写入了查重库（只有"通过"类结论才写）。
	Applied bool `json:"applied"`
}

// reviewStore 维护复核结论与查重库回写。
type reviewStore struct {
	mu      sync.Mutex
	seq     int
	records []*ReviewOutcome
	// extra 是复核回写产生的历史记录，键为 claim_no。
	extra map[string]reviewHistory
	now   func() time.Time
}

type reviewHistory struct {
	Rec *stubHistory
}

// stubHistory 只是占位，真正的 schema.HistRecord 由 server 层转换；
// 这里保存原始字段，避免 reviewStore 依赖 schema 包。
type stubHistory struct {
	ClaimNo     string
	InvoiceNo   string
	ApplicantID string
	SellerTaxID string
	IssueDate   string
	AmountFen   int64
	Category    string
	Status      string
	SubmitDate  string
}

func newReviewStore() *reviewStore {
	return &reviewStore{extra: map[string]reviewHistory{}, now: time.Now}
}

// Add 追加一条复核结论，并返回需要写入查重库的记录描述。
//
// 不在这里直接改 Server 的数据结构，而是把"该写什么"返回给调用方，
// 让回写逻辑只有一处（Server.AddReview），撤销与提交走同一份代码。
func (s *reviewStore) Add(r ReviewOutcome) ReviewOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	r.ID = fmt.Sprintf("RV%04d", s.seq)
	if r.At.IsZero() {
		r.At = s.now()
	}
	s.records = append(s.records, &r)
	return r
}

// Get 按编号取。
func (s *reviewStore) Get(id string) (ReviewOutcome, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		if r.ID == id {
			return *r, true
		}
	}
	return ReviewOutcome{}, false
}

// Revoke 撤销一条结论。already 为真表示之前就撤销过。
func (s *reviewStore) Revoke(id, by, byName, reason string) (ReviewOutcome, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		if r.ID != id {
			continue
		}
		if r.Revoked() {
			return *r, true, nil
		}
		t := s.now()
		r.RevokedAt = &t
		r.RevokedBy = by
		r.RevokedByName = byName
		r.RevokeReason = reason
		return *r, false, nil
	}
	return ReviewOutcome{}, false, fmt.Errorf("复核记录不存在: %s", id)
}

// UpdateAffected 记录一条结论对查重库的影响，撤销时按此回滚。
func (s *reviewStore) UpdateAffected(id string, ref HistoryRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		if r.ID == id {
			r.AffectedHistory = ref
			return
		}
	}
}

// List 返回全部结论，最近的在前。撤销的也返回（审计要看）。
func (s *reviewStore) List() []ReviewOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ReviewOutcome, 0, len(s.records))
	for i := len(s.records) - 1; i >= 0; i-- {
		out = append(out, *s.records[i])
	}
	return out
}

// Active 返回未撤销的结论。
func (s *reviewStore) Active() []ReviewOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ReviewOutcome
	for _, r := range s.records {
		if !r.Revoked() {
			out = append(out, *r)
		}
	}
	return out
}

// ---- 查重库回写 ----

// PutHistory 写入一条回写记录。已存在则覆盖（同一张单多次复核，以最后一次为准）。
func (s *reviewStore) PutHistory(h stubHistory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extra[h.ClaimNo] = reviewHistory{Rec: &h}
}

// DropHistory 撤掉一条回写记录。
func (s *reviewStore) DropHistory(claimNo string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.extra[claimNo]; !ok {
		return false
	}
	delete(s.extra, claimNo)
	return true
}

// Histories 返回全部回写记录，按 claim_no 排序（稳定输出）。
func (s *reviewStore) Histories() []stubHistory {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]stubHistory, 0, len(s.extra))
	for _, v := range s.extra {
		out = append(out, *v.Rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClaimNo < out[j].ClaimNo })
	return out
}

// HistoryCount 返回回写记录条数。
func (s *reviewStore) HistoryCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.extra)
}
