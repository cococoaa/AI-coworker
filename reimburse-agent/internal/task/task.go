// Package task 是报销单的任务生命周期。
//
// 这是"原型"与"能持续承接工作的 AI 同事"之间那道分界线：
//
//	原型：选一张单 → 跑一遍 → 出结论 → 结束（后续不知道去哪了）
//	同事：知道这张单现在卡在哪、卡在谁那里、缺什么、补了之后要重查哪几项
//
// 设计要点：
//
//  1. **状态机是可枚举的**，不允许隐式状态。每张单在任一时刻只能是七种状态之一。
//  2. **补件后续审，不是重跑。** 已核验通过且未过期的结果保留，
//     只重查"因缺件而跳过"的判定项——这依赖于判定结果里的 Skipped 与 FieldStatus。
//  3. **每次审核留批次号。** 同一张单可能审多次（补件续审、复核后重审），
//     每次都要能翻出来当时查到了什么。
//  4. **落盘。** 服务重启后仍知道每张单处理到哪一步。用 JSON 文件，不引入数据库。
package task

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

// State 是任务状态。
type State string

const (
	// Pending 已受理，尚未审核。
	Pending State = "pending"
	// Auditing 审核中。
	Auditing State = "auditing"
	// NeedMore 退回补材料——等员工补交，是"卡在对方那里"。
	NeedMore State = "need_more"
	// NeedManual 转人工复核——等财务判断，是"卡在我们这里"。
	NeedManual State = "need_manual"
	// Approved 初审通过。
	Approved State = "approved"
	// Rejected 驳回。
	Rejected State = "rejected"
	// Withdrawn 撤销/作废。
	Withdrawn State = "withdrawn"
)

// AllStates 按流程顺序列出，供界面与校验使用。
func AllStates() []State {
	return []State{Pending, Auditing, NeedMore, NeedManual, Approved, Rejected, Withdrawn}
}

// Valid 判断状态取值是否合法。
func (s State) Valid() bool {
	for _, x := range AllStates() {
		if x == s {
			return true
		}
	}
	return false
}

// Terminal 表示不再需要推进。
func (s State) Terminal() bool { return s == Approved || s == Rejected || s == Withdrawn }

// WaitingOn 说明这个状态在等谁。这是"知道任务卡在哪"的直接体现。
func (s State) WaitingOn() string {
	switch s {
	case NeedMore:
		return "报销人补交材料"
	case NeedManual:
		return "财务复核"
	case Auditing:
		return "系统审核中"
	case Pending:
		return "等待开始审核"
	case Approved, Rejected:
		return "已结束"
	case Withdrawn:
		return "已撤销"
	}
	return "未知"
}

func (s State) String() string { return string(s) }

// Label 是给财务看的中文名。
func (s State) Label() string {
	switch s {
	case Pending:
		return "待审核"
	case Auditing:
		return "审核中"
	case NeedMore:
		return "待补材料"
	case NeedManual:
		return "待人工复核"
	case Approved:
		return "初审通过"
	case Rejected:
		return "已驳回"
	case Withdrawn:
		return "已撤销"
	}
	return string(s)
}

// MissingItem 是一条缺失材料。
//
// 关键区分（与判定层的字段四态对应）：
//   - Kind=absent    原件确实没有这一项 → 让员工补交
//   - Kind=unreadable 有这一项但看不清 → 让员工重新提供清晰的原件
//   - Kind=ambiguous 两模型结果不一致 → 需人工核对，补交也没用
//
// 这三种在界面上必须分开显示，因为它们要求的动作不同。
type MissingItem struct {
	// Key 是唯一标识（字段名或材料名），补交时按它匹配。
	Key string `json:"key"`
	// Label 是给人看的名称。
	Label string `json:"label"`
	// Kind 取 absent / unreadable / ambiguous / missing_attachment。
	Kind string `json:"kind"`
	// Reason 说明为什么要它，引用具体条款。
	Reason string `json:"reason"`
	// Clause 是依据的条款编号。
	Clause string `json:"clause,omitempty"`
	// Hint 告诉报销人这一项该怎么填（键名与格式）。
	// 没有它，界面上的输入框就是个空白框，用户不知道该写什么。
	Hint string `json:"hint,omitempty"`
	// Resolved 为真表示已补交。
	Resolved bool `json:"resolved"`
	// ResolvedAt 是补交时间。
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	// Version 是该材料当前的版本号（补交一次加一）。
	Version int `json:"version"`
}

// Attachment 是一份上传材料。
type Attachment struct {
	// Kind 取 invoice / itinerary / folio / consumption / approval。
	Kind string `json:"kind"`
	// Name 是文件名（展示用）。
	Name string `json:"name"`
	// Version 从 1 开始，补交时递增。
	Version int `json:"version"`
	// At 是上传时间。
	At time.Time `json:"at"`
	// By 是上传人工号。
	By string `json:"by,omitempty"`
	// Note 是说明（例如"补交出差审批单"）。
	Note string `json:"note,omitempty"`
	// Content 是补交材料的正文（首版只支持文本，PDF/图片后续接 OCR）。
	// 续审时用它替代原来缺失的那份文档，这样补交才真正影响判定。
	Content string `json:"content,omitempty"`
	// Replaces 表示这份材料替代哪一项（对应 MissingItem.Key）。
	Replaces string `json:"replaces,omitempty"`
	// Hash 是 Content 的摘要，用于识别"同一份材料被重复提交"。
	Hash string `json:"hash,omitempty"`
}

// Recheck 记录续审时要重查什么。这是"增量续审"的核心。
type Recheck struct {
	// CheckIDs 是因缺件而跳过、补交后需要重新判定的判定项。
	CheckIDs []string `json:"check_ids,omitempty"`
	// KeepVerified 是续审时可以复用的已核验事实（不重查）。
	KeepVerified []string `json:"keep_verified,omitempty"`
	// Reverify 是需要重新核验的（结果有时效性，例如发票查验）。
	Reverify []string `json:"reverify,omitempty"`
	// Reason 是一句话说明为什么这样安排。
	Reason string `json:"reason,omitempty"`
}

// Run 是一次审核批次。
type Run struct {
	// ID 是批次号。
	ID string `json:"id"`
	// Round 从 1 开始，补件续审会加一。
	Round int `json:"round"`
	// At 是开始时间。
	At time.Time `json:"at"`
	// Trigger 取 initial / resubmit / manual_recheck。
	Trigger string `json:"trigger"`
	// Decision 是本批次的结论。
	Decision string `json:"decision"`
	// Hits 是本轮命中的判定项。
	//
	// 序列化成空数组而不是 null：Go 的 encoding/json 对 nil slice 永远输出 null，
	// 光靠 tag 改不掉。所以写入前用 Normalize() 把它填成 []string{}，
	// 让"命中 0 项（干净通过）"与"没有记录"在 JSON 里能分清。
	Hits []string `json:"hits"`
	// Skipped 是因缺件等原因未判定的项及原因。
	Skipped map[string]string `json:"skipped,omitempty"`
	// ApprovedFen 是核准金额。
	ApprovedFen int64 `json:"approved_fen"`
	// CostUSD 是本批次成本。
	CostUSD float64 `json:"cost_usd"`
	// ElapsedMS 是耗时。
	ElapsedMS int64 `json:"elapsed_ms"`
	// Recheck 记录本批次是怎么安排重查的（补件续审时有值）。
	Recheck *Recheck `json:"recheck,omitempty"`
}

// Event 是任务事件，用于回放"这张单经历了什么"。
type Event struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"` // created / audited / need_more / resubmitted / manual / approved / rejected / withdrawn
	Detail string    `json:"detail"`
	By     string    `json:"by,omitempty"`
	Round  int       `json:"round,omitempty"`
}

// Task 是一张报销单的任务。
type Task struct {
	ID string `json:"id"`
	// CaseID 关联到用例/单据编号。
	CaseID string `json:"case_id"`
	// ClaimNo 是报销单号。
	ClaimNo string `json:"claim_no"`
	// Applicant 是提交人工号。
	Applicant string `json:"applicant,omitempty"`

	State State `json:"state"`
	// CreatedAt / UpdatedAt 便于算"卡了多久"。
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Missing 是补件清单。
	Missing []MissingItem `json:"missing,omitempty"`
	// Attachments 是材料版本历史。
	Attachments []Attachment `json:"attachments,omitempty"`
	// Runs 是审核批次。
	Runs []Run `json:"runs,omitempty"`
	// Events 是任务事件。
	Events []Event `json:"events,omitempty"`

	// NextRecheck 是下次续审要重查什么（由上一次判定结果算出）。
	NextRecheck *Recheck `json:"next_recheck,omitempty"`
	// LastDecision / LastApprovedFen 是最近一次结论，便于列表展示。
	LastDecision    string `json:"last_decision,omitempty"`
	LastApprovedFen int64  `json:"last_approved_fen"`
}

// Outstanding 返回未解决的缺失项。
func (t *Task) Outstanding() []MissingItem {
	out := make([]MissingItem, 0, len(t.Missing))
	for _, m := range t.Missing {
		if !m.Resolved {
			out = append(out, m)
		}
	}
	return out
}

// ResolvedAll 表示所有缺失项都已补交。
func (t *Task) ResolvedAll() bool { return len(t.Outstanding()) == 0 }

// WaitingDays 返回当前状态停留了几天。
func (t *Task) WaitingDays(now time.Time) int {
	return int(now.Sub(t.UpdatedAt).Hours() / 24)
}

// Store 是任务存储。落盘为单个 JSON 文件。
type Store struct {
	path string
	mu   sync.Mutex
	data storeData
	now  func() time.Time
	seq  int
}

type storeData struct {
	Version int              `json:"version"`
	Note    string           `json:"note,omitempty"`
	Tasks   map[string]*Task `json:"tasks"`
}

// Open 打开或创建任务库。
func Open(path string) (*Store, error) {
	s := &Store{path: path, now: time.Now, data: storeData{Version: 1, Tasks: map[string]*Task{}}}
	b, err := os.ReadFile(path)
	if err == nil {
		var d storeData
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("任务库损坏 %s: %w", path, err)
		}
		if d.Tasks == nil {
			d.Tasks = map[string]*Task{}
		}
		d.Version = 1
		s.data = d
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	// 序列号从已有任务里推，避免重启后 ID 撞车。
	for id := range s.data.Tasks {
		var n int
		if _, err := fmt.Sscanf(id, "T%04d", &n); err == nil && n > s.seq {
			s.seq = n
		}
	}
	return s, nil
}

// Create 新建任务。同一张单据已有任务时应先 FindByCase 复用，这里只做兜底拒绝。
func (s *Store) Create(caseID, claimNo, applicant string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.data.Tasks {
		if t.CaseID == caseID && !t.State.Terminal() {
			return nil, fmt.Errorf("该单据已有进行中的任务: %s", t.ID)
		}
	}
	s.seq++
	t := &Task{
		ID: fmt.Sprintf("T%04d", s.seq), CaseID: caseID, ClaimNo: claimNo, Applicant: applicant,
		State: Pending, CreatedAt: s.now(), UpdatedAt: s.now(),
	}
	t.Events = append(t.Events, Event{At: s.now(), Kind: "created",
		Detail: "受理报销单 " + claimNo})
	s.data.Tasks[t.ID] = t
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	cp := *t
	return &cp, nil
}

// Get 按任务号取（返回副本，调用方改不了内部状态）。
func (s *Store) Get(id string) (*Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.data.Tasks[id]
	if !ok {
		return nil, false
	}
	cp := *t
	cp.Missing = append([]MissingItem{}, t.Missing...)
	cp.Attachments = append([]Attachment{}, t.Attachments...)
	cp.Runs = append([]Run{}, t.Runs...)
	cp.Events = append([]Event{}, t.Events...)
	return &cp, true
}

// FindByCase 按单据找最近的任务。
func (s *Store) FindByCase(caseID string) (*Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *Task
	for _, t := range s.data.Tasks {
		if t.CaseID != caseID {
			continue
		}
		if best == nil || t.CreatedAt.After(best.CreatedAt) {
			best = t
		}
	}
	if best == nil {
		return nil, false
	}
	cp := *best
	return &cp, true
}

// List 返回全部任务，按更新时间倒序（最近有动静的在前）。
func (s *Store) List() []*Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Task, 0, len(s.data.Tasks))
	for _, t := range s.data.Tasks {
		cp := *t
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// Stats 返回按状态统计的数量，供"今日待办"用。
func (s *Store) Stats() map[State]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[State]int{}
	for _, t := range s.data.Tasks {
		out[t.State]++
	}
	return out
}

// Update 在一个事务里修改任务。fn 返回错误则整体不生效（不落盘）。
func (s *Store) Update(id string, fn func(t *Task) error) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.data.Tasks[id]
	if !ok {
		return nil, fmt.Errorf("任务不存在: %s", id)
	}
	// 在副本上操作，成功才替换，保证失败不改内存。
	work := *t
	work.Missing = append([]MissingItem{}, t.Missing...)
	work.Attachments = append([]Attachment{}, t.Attachments...)
	work.Runs = append([]Run{}, t.Runs...)
	work.Events = append([]Event{}, t.Events...)
	if err := fn(&work); err != nil {
		return nil, err
	}
	work.UpdatedAt = s.now()
	*t = work
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	cp := work
	return &cp, nil
}

// saveLocked 原子落盘。
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// SetNow 注入时钟，测试用。
func (s *Store) SetNow(f func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = f
}

// ---- 状态迁移 ----

// ApplyDecision 把一次审核结论落到任务状态上。
//
// 映射关系：
//
//	approve / approve_with_adjustment → Approved
//	reject                            → Rejected
//	need_more_info                    → NeedMore（并给出补件清单）
//	manual_review                     → NeedManual
func (s *Store) ApplyDecision(id string, run Run, decision string, missing []MissingItem, recheck *Recheck, by string) (*Task, error) {
	return s.Update(id, func(t *Task) error {
		run.Round = len(t.Runs) + 1
		if run.At.IsZero() {
			run.At = s.now()
		}
		if run.ID == "" {
			run.ID = fmt.Sprintf("%s-R%d", t.ID, run.Round)
		}
		if run.Trigger == "" {
			run.Trigger = "initial"
		}
		run = run.Normalize()
		t.Runs = append(t.Runs, run)
		t.LastDecision = decision
		t.LastApprovedFen = run.ApprovedFen

		switch decision {
		case "approve", "approve_with_adjustment":
			t.State = Approved
			t.Missing = nil
			t.NextRecheck = nil
			t.Events = append(t.Events, Event{At: s.now(), Kind: "approved",
				Detail: fmt.Sprintf("第 %d 轮审核通过，核准 %d 分", run.Round, run.ApprovedFen), By: by, Round: run.Round})
		case "reject":
			t.State = Rejected
			t.Missing = nil
			t.NextRecheck = nil
			t.Events = append(t.Events, Event{At: s.now(), Kind: "rejected",
				Detail: "第 " + fmt.Sprint(run.Round) + " 轮审核驳回", By: by, Round: run.Round})
		case "need_more_info":
			t.State = NeedMore
			t.Missing = missing
			t.NextRecheck = recheck
			var names []string
			for _, m := range missing {
				names = append(names, m.Label)
			}
			t.Events = append(t.Events, Event{At: s.now(), Kind: "need_more",
				Detail: "退回补材料：" + strings.Join(names, "、"), By: by, Round: run.Round})
		case "manual_review":
			t.State = NeedManual
			t.Missing = missing
			t.NextRecheck = recheck
			t.Events = append(t.Events, Event{At: s.now(), Kind: "manual",
				Detail: "转人工复核", By: by, Round: run.Round})
		default:
			return fmt.Errorf("未知结论: %s", decision)
		}
		return nil
	})
}

// Resubmit 登记一次补交，并把任务送回待审（等待续审）。
// kind 是补交的材料类型；它必须能匹配到某条未解决的缺失项。
func (s *Store) Resubmit(id, kind, name, by, note, content string) (*Task, error) {
	t, _, err := s.ResubmitIdem(id, kind, name, by, note, content)
	return t, err
}

// ResubmitIdem 登记补交，并保证重复提交是幂等的。
//
// 什么叫重复：同一项材料、同样的正文。双击提交、浏览器重试、报销人没看到
// "已提交"又点一次，都会撞上这种情况。此时应该当作成功、不新增版本——
// 既不报错（用户会以为丢了），也不新增附件（版本历史会被噪声污染）。
//
// 第二个返回值 dup 为真表示这次是重复提交、没有改动任何数据。
func (s *Store) ResubmitIdem(id, kind, name, by, note, content string) (*Task, bool, error) {
	dup := false
	var res *Task
	res, err := s.Update(id, func(t *Task) error {
		h := contentHash(content)

		// 先判是否已提交过同样的内容。注意要在状态检查之前判：
		// 补齐后状态已变成"待审核"，重复提交仍应视为成功。
		for i := range t.Missing {
			m := &t.Missing[i]
			if m.Key != kind || !m.Resolved {
				continue
			}
			for j := range t.Attachments {
				a := t.Attachments[j]
				if a.Replaces == m.Key && a.Hash == h {
					dup = true
					return nil
				}
			}
		}

		// 找到匹配的缺失项：先找未解决的，再找已解决但允许替换的。
		matched := -1
		replace := false
		for i := range t.Missing {
			m := &t.Missing[i]
			if m.Resolved {
				continue
			}
			if m.Key == kind || m.Kind == "missing_attachment" && kind == "approval" {
				matched = i
				break
			}
		}
		if matched < 0 && t.State == Pending {
			// 已交但还没续审：允许整份替换。报销人传错文件是常事，
			// 拦着不让改，就只能等续审出个错结论再吵。
			for i := range t.Missing {
				if t.Missing[i].Key == kind {
					matched, replace = i, true
					break
				}
			}
		}
		if matched < 0 && t.State != NeedMore {
			return fmt.Errorf("当前状态 %s 不需要补交（只有待补材料才接受补交）", t.State.Label())
		}
		if matched < 0 {
			var keys []string
			for _, m := range t.Outstanding() {
				keys = append(keys, m.Key)
			}
			return fmt.Errorf("补交的 %q 不在待补清单里（待补：%s）", kind, strings.Join(keys, "、"))
		}
		m := &t.Missing[matched]
		now := s.now()
		if !m.Resolved {
			m.Resolved = true
			m.ResolvedAt = &now
		}
		m.Version++
		t.Attachments = append(t.Attachments, Attachment{
			Kind: kind, Name: name, Version: m.Version, At: now, By: by, Note: note,
			Content: content, Replaces: m.Key, Hash: h,
		})
		if replace {
			t.Events = append(t.Events, Event{At: now, Kind: "resubmitted",
				Detail: "替换了已提交的 " + m.Label + "（现为第 " + fmt.Sprint(m.Version) + " 版），等待续审", By: by})
			return nil
		}
		// 全部补齐 → 回到待审，等续审。
		if t.ResolvedAll() {
			t.State = Pending
			t.Events = append(t.Events, Event{At: now, Kind: "resubmitted",
				Detail: "材料已补齐（" + m.Label + "），等待续审", By: by})
		} else {
			t.Events = append(t.Events, Event{At: now, Kind: "resubmitted",
				Detail: "已补交 " + m.Label + "，尚有 " + fmt.Sprint(len(t.Outstanding())) + " 项待补", By: by})
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return res, dup, nil
}

// Normalize 把可空字段填成空值形式，保证 JSON 输出稳定。
//
// 只做这一件事：Hits 为 nil 时填成空切片，序列化成 [] 而不是 null。
// 界面上"命中 0 项"和"没有命中记录"是两种意思，审计也依赖这个差别。
func (r Run) Normalize() Run {
	if r.Hits == nil {
		r.Hits = []string{}
	}
	return r
}

// contentHash 是补交正文的摘要，用于判重。
func contentHash(s string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(s)))
	return hex.EncodeToString(h[:8])
}

// LatestContent 返回某项缺失材料的最新补交正文。
// 续审时用它替代原来缺失的文档；没有补交则返回空。
func (t *Task) LatestContent(key string) (string, bool) {
	var best *Attachment
	for i := range t.Attachments {
		a := &t.Attachments[i]
		if a.Replaces != key || a.Content == "" {
			continue
		}
		if best == nil || a.Version > best.Version {
			best = a
		}
	}
	if best == nil {
		return "", false
	}
	return best.Content, true
}

// AttachmentOf 返回某类材料的最新一版（用于补交佐证凭证）。
func (t *Task) AttachmentOf(kind string) (Attachment, bool) {
	var best *Attachment
	for i := range t.Attachments {
		a := &t.Attachments[i]
		if a.Kind != kind || a.Content == "" {
			continue
		}
		if best == nil || a.Version > best.Version {
			best = a
		}
	}
	if best == nil {
		return Attachment{}, false
	}
	return *best, true
}

// BeginAudit 抢占任务，把它标成"审核中"。
//
// 为什么需要显式抢占：连续点两次"续审"，两次请求几乎同时到达，
// 都读到"待审核"就都会启动一轮审核。虽然第二次跑完会覆盖第一次的结论，
// 但事件流、成本、批次号都会重复记一遍——看起来像系统抽风。
// 这里用状态本身做互斥：只有从可审核状态成功切到 Auditing 的那次才算抢到。
func (s *Store) BeginAudit(id string) (*Task, error) {
	return s.Update(id, func(t *Task) error {
		switch t.State {
		case Pending:
			t.State = Auditing
			t.Events = append(t.Events, Event{At: s.now(), Kind: "auditing", Detail: "开始审核"})
			return nil
		case Auditing:
			return fmt.Errorf("该任务正在审核中，请等这一轮结束（任务 %s）", t.ID)
		default:
			return fmt.Errorf("当前状态「%s」不能开始审核", t.State.Label())
		}
	})
}

// ResolveManual 把"待人工复核"的任务按财务结论收口。
//
// 为什么必须做：审核转人工后，任务停在 NeedManual，卡在"财务复核"。
// 财务点了通过，如果任务状态不动，待办列表里这张单永远挂着——
// 财务会以为自己没点成功，再点一次。这是"复核结论没有回落到流程"的典型表现。
//
// 返回的错误用于区分"状态不允许"，由调用方决定是提示还是忽略。
func (s *Store) ResolveManual(id, decision, by, note string) (*Task, error) {
	return s.Update(id, func(t *Task) error {
		if t.State != NeedManual {
			// 不是转人工的单，复核结论照样记在复核库里，但不改任务状态。
			// 这里返回错误让调用方知道"没落上"，而不是假装成功。
			return fmt.Errorf("任务 %s 当前为 %s，不是待人工复核，未变更状态", t.ID, t.State.Label())
		}
		var next State
		switch decision {
		case "approve", "approve_with_adjustment":
			next = Approved
		case "reject":
			next = Rejected
		case "need_more_info":
			next = NeedMore
		default:
			// manual_review 保持原状：财务只是留了个意见，还没定。
			return nil
		}
		t.State = next
		t.Missing = nil
		t.NextRecheck = nil
		// 最近结论跟着人定的走：否则会出现"状态=初审通过、结论=转人工复核"
		// 这种自相矛盾的记录，界面上看的人会以为数据错了。
		t.LastDecision = decision
		detail := "财务复核：" + next.Label()
		if note != "" {
			detail += "（" + note + "）"
		}
		t.Events = append(t.Events, Event{At: s.now(), Kind: "reviewed", Detail: detail, By: by})
		return nil
	})
}

// Withdraw 撤销任务。
func (s *Store) Withdraw(id, by, reason string) (*Task, error) {
	return s.Update(id, func(t *Task) error {
		if t.State == Withdrawn {
			return nil // 幂等
		}
		t.State = Withdrawn
		t.Events = append(t.Events, Event{At: s.now(), Kind: "withdrawn",
			Detail: "撤销：" + reason, By: by})
		return nil
	})
}
