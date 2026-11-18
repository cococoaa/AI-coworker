package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	// IdemKey 是幂等键：同一单据、同一结论、同一操作人、同一意见重复提交时，
	// 命中同一条记录而不是新增一条。界面双击、网络重试都不会留下两条。
	IdemKey string `json:"idem_key,omitempty"`
	// Duplicate 为真表示本次提交命中了已有记录（幂等），没有新增。
	// 只用于响应，不落盘。
	Duplicate bool `json:"duplicate,omitempty"`
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
	mu sync.Mutex
	// path 非空时把记录落盘：复核结论会改变查重结果，重启后不能凭空消失。
	path    string
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

// reviewFile 是落盘格式。版本号留出来，便于以后改结构时识别旧文件。
type reviewFile struct {
	Version int              `json:"version"`
	Note    string           `json:"note,omitempty"`
	Seq     int              `json:"seq"`
	Records []*ReviewOutcome `json:"records"`
	Extra   []stubHistory    `json:"extra,omitempty"`
}

// OpenPath 打开（或创建）复核记录文件，并立刻落一次盘确认可写。
//
// 为什么要落盘：复核结论会写进查重库，影响后续单据的重复报销判定。
// 只存在内存里，重启后"这张票已经报过"这个事实就丢了——那记忆是假的。
func (s *reviewStore) OpenPath(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path = path
	b, err := os.ReadFile(path)
	if err == nil {
		var f reviewFile
		if err := json.Unmarshal(b, &f); err != nil {
			return fmt.Errorf("复核记录文件损坏 %s: %w", path, err)
		}
		s.records = f.Records
		s.seq = f.Seq
		for i := range s.records {
			if n := len(s.records[i].ID); n == 6 {
				var k int
				if _, err := fmt.Sscanf(s.records[i].ID, "RV%04d", &k); err == nil && k > s.seq {
					s.seq = k
				}
			}
		}
		for _, h := range f.Extra {
			hh := h
			s.extra[hh.ClaimNo] = reviewHistory{Rec: &hh}
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return s.saveLocked()
}

// saveLocked 落盘。调用方必须持锁。
func (s *reviewStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	f := reviewFile{Version: 1, Note: "复核结论与查重回写。撤销为软删除，记录保留。",
		Seq: s.seq, Records: s.records}
	for _, v := range s.extra {
		f.Extra = append(f.Extra, *v.Rec)
	}
	sort.Slice(f.Extra, func(i, j int) bool { return f.Extra[i].ClaimNo < f.Extra[j].ClaimNo })
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ComputeIdemKey 计算一条复核结论的幂等键。
// 只取"决定这条结论是什么"的字段，不看提交时间与来源 IP。
func (r ReviewOutcome) ComputeIdemKey() string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		"review", r.CaseID, r.ClaimNo, r.Decision, r.Operator, strings.TrimSpace(r.Note),
	}, "|")))
	return hex.EncodeToString(h[:8])
}

// Add 追加一条复核结论，并返回需要写入查重库的记录描述。
//
// 不在这里直接改 Server 的数据结构，而是把"该写什么"返回给调用方，
// 让回写逻辑只有一处（Server.AddReview），撤销与提交走同一份代码。
// AddIdem 追加一条复核结论；同一幂等键已存在（且未撤销）时返回已有记录。
// 第二个返回值为真表示这次是重复提交、没有新增记录。
func (s *reviewStore) AddIdem(r ReviewOutcome) (ReviewOutcome, bool) {
	out, dup, err := s.AddIdemErr(r)
	if err != nil {
		// 兼容路径：调用方没处理错误时，这里也不能把半成功的记录留在内存里。
		return out, dup
	}
	return out, dup
}

// AddIdemErr 是带错误返回的版本。落盘失败时回滚内存，不留下"内存有、盘上没有"的记录。
func (s *reviewStore) AddIdemErr(r ReviewOutcome) (ReviewOutcome, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := r.ComputeIdemKey()
	for _, old := range s.records {
		if old.IdemKey == key && !old.Revoked() {
			return *old, true, nil
		}
	}
	prevSeq := s.seq
	s.seq++
	r.ID = fmt.Sprintf("RV%04d", s.seq)
	r.IdemKey = key
	if r.At.IsZero() {
		r.At = s.now()
	}
	s.records = append(s.records, &r)
	if err := s.saveLocked(); err != nil {
		// 回滚：序号与记录都退回去，磁盘和内存都不留半截。
		s.seq = prevSeq
		s.records = s.records[:len(s.records)-1]
		return ReviewOutcome{}, false, fmt.Errorf("复核记录落盘失败，本次结论未生效: %w", err)
	}
	return r, false, nil
}

// AddWithHistory 在同一个临界区里写复核记录与查重回写，一次落盘。
//
// 为什么必须一起写：先写记录、再写查重，第二次失败会留下"记录在、查重没写"——
// 界面显示已通过，但同一张票下次还能再报。反过来也一样。
// 要么两条都进盘，要么都不进。
func (s *reviewStore) AddWithHistory(r ReviewOutcome, put *stubHistory) (ReviewOutcome, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := r.ComputeIdemKey()
	for _, old := range s.records {
		if old.IdemKey == key && !old.Revoked() {
			return *old, true, nil
		}
	}

	prevSeq := s.seq
	var prevExtra reviewHistory
	var hadExtra bool
	if put != nil {
		prevExtra, hadExtra = s.extra[put.ClaimNo]
	}

	s.seq++
	r.ID = fmt.Sprintf("RV%04d", s.seq)
	r.IdemKey = key
	if put != nil {
		// 查重记录按复核记录编号命名：同一张单多次通过各留一条，
		// 撤销其中一条只撤自己那条，不会把别人的一起删掉。
		put.ClaimNo = put.ClaimNo + "#" + r.ID
		r.AffectedHistory = HistoryRef{ClaimNo: put.ClaimNo, InvoiceNo: put.InvoiceNo, Applied: true}
	}
	if r.At.IsZero() {
		r.At = s.now()
		if put != nil {
			put.SubmitDate = r.At.Format("2006-01-02")
		}
	}
	s.records = append(s.records, &r)
	if put != nil {
		h := *put
		s.extra[h.ClaimNo] = reviewHistory{Rec: &h}
	}

	if err := s.saveLocked(); err != nil {
		// 整体回滚，内存里不留任何半截状态。
		s.seq = prevSeq
		s.records = s.records[:len(s.records)-1]
		if put != nil {
			if hadExtra {
				s.extra[put.ClaimNo] = prevExtra
			} else {
				delete(s.extra, put.ClaimNo)
			}
		}
		return ReviewOutcome{}, false, fmt.Errorf("复核结论落盘失败，本次操作未生效: %w", err)
	}
	return r, false, nil
}

// UpdateAffectedErr 记录一条结论对查重库的影响；落盘失败时还原。
func (s *reviewStore) UpdateAffectedErr(id string, ref HistoryRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		if r.ID != id {
			continue
		}
		old := r.AffectedHistory
		r.AffectedHistory = ref
		if err := s.saveLocked(); err != nil {
			r.AffectedHistory = old
			return fmt.Errorf("复核记录落盘失败: %w", err)
		}
		return nil
	}
	return nil
}

// Add 追加一条复核结论（不判重，保留给内部一次性写入）。
func (s *reviewStore) Add(r ReviewOutcome) ReviewOutcome {
	out, _, _ := s.AddIdemErr(r)
	return out
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
		// 记住撤销前的状态，落盘失败时还原——否则内存显示"已撤销"、盘上还是"有效"。
		oldAt, oldBy, oldByName, oldReason := r.RevokedAt, r.RevokedBy, r.RevokedByName, r.RevokeReason
		t := s.now()
		r.RevokedAt = &t
		r.RevokedBy = by
		r.RevokedByName = byName
		r.RevokeReason = reason
		if err := s.saveLocked(); err != nil {
			r.RevokedAt, r.RevokedBy, r.RevokedByName, r.RevokeReason = oldAt, oldBy, oldByName, oldReason
			return ReviewOutcome{}, false, fmt.Errorf("撤销落盘失败，本次撤销未生效: %w", err)
		}
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
	out := make([]ReviewOutcome, 0, len(s.records))
	for _, r := range s.records {
		if !r.Revoked() {
			out = append(out, *r)
		}
	}
	return out
}

// ---- 查重库回写 ----

// PutHistory 写入一条回写记录。已存在则覆盖（同一张单多次复核，以最后一次为准）。
// 落盘失败时还原旧值并返回错误：查重库决定了"这张票能不能再报"，不能只改内存。
func (s *reviewStore) PutHistory(h stubHistory) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, had := s.extra[h.ClaimNo]
	s.extra[h.ClaimNo] = reviewHistory{Rec: &h}
	if err := s.saveLocked(); err != nil {
		if had {
			s.extra[h.ClaimNo] = old
		} else {
			delete(s.extra, h.ClaimNo)
		}
		return fmt.Errorf("查重回写落盘失败: %w", err)
	}
	return nil
}

// DropHistory 撤掉一条回写记录。落盘失败时还原。
func (s *reviewStore) DropHistory(claimNo string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.extra[claimNo]
	if !ok {
		return false, nil
	}
	delete(s.extra, claimNo)
	if err := s.saveLocked(); err != nil {
		s.extra[claimNo] = old
		return false, fmt.Errorf("撤回查重记录落盘失败: %w", err)
	}
	return true, nil
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
