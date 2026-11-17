// Package server 提供单页演示界面与 SSE 事件流。
//
// 设计原则：
//   - 一次审核 = 一个 session。事件在审核过程中边产生边推给浏览器，不是跑完再回放；
//   - 浏览器只负责渲染，不参与判定。刷新页面能从 session 里重放已发生的事件；
//   - 演示开关（关闭模型、注入工具故障）都是请求参数，随每次审核记录在案，事后可复现；
//   - 复核结论写回历史库，下一张单据的查重能用到，形成"记忆"闭环。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"reimburse/internal/audit"
	"reimburse/internal/identity"
	"reimburse/internal/llm"
	"reimburse/internal/quota"
	"reimburse/internal/schema"
	"reimburse/plugins/reimburse-audit/policy"
)

// CaseIndex 是一条用例的摘要，供界面列表使用。
type CaseIndex struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Category string   `json:"category"`
	Tags     []string `json:"tags"`
	// ExpectedDecision 是标准答案。界面上默认隐藏，勾选"显示标准答案"才出现。
	ExpectedDecision string `json:"expected_decision"`
}

// Session 是一次审核的运行记录。
type Session struct {
	ID      string
	CaseID  string
	Started time.Time
	// QuotaClient 是调用方标识（令牌名），审核完成后按它分桶记账。
	QuotaClient string

	mu     sync.Mutex
	events []audit.Event
	report *audit.Report
	err    string
	done   chan struct{}
	// listeners 是正在订阅的 SSE 连接。
	listeners map[chan audit.Event]bool
}

// Server 是 HTTP 服务。
type Server struct {
	Root  string // 数据根目录（含 cases/）
	Cases []*schema.Case
	RF    *policy.File
	Cfg   llm.Config
	// MCPURL 为空表示进程内起 MCP 服务；否则连远程服务。
	MCPURL string
	// MCPToken 是连远程 MCP 服务用的令牌。
	MCPToken string
	// MCPData 是进程内 MCP 服务的数据文件。
	MCPData string

	mu       sync.Mutex
	sessions map[string]*Session
	// reviews 是复核结论与查重库回写（支持撤销）。
	reviews *reviewStore
	// People 是工号名单；为空时拒绝所有写入类操作（默认拒绝）。
	People *identity.Registry
	// Quota 是周预算；为空时不限制。
	Quota *quota.Store
	// llmClient 延迟创建，便于测试替换。
	llmClient *llm.Client
	// ReplayDir 不为空时，实时调用失败就回放录制；界面会标明"回放"。
	ReplayDir string
	// DemoSynthetic 为真时，连录制都没有就用合成抽取。只在无密钥演示时开启。
	DemoSynthetic bool
}

// New 创建服务。
func New(root string, rf *policy.File, cfg llm.Config) (*Server, error) {
	cases, err := schema.LoadAll(root)
	if err != nil {
		return nil, err
	}
	s := &Server{Root: root, Cases: cases, RF: rf, Cfg: cfg,
		sessions: map[string]*Session{}, reviews: newReviewStore()}
	return s, nil
}

// Index 返回用例列表。
func (s *Server) Index() []CaseIndex {
	out := make([]CaseIndex, 0, len(s.Cases))
	for _, c := range s.Cases {
		out = append(out, CaseIndex{ID: c.ID, Title: c.Truth.Title, Category: c.Claim.Category,
			Tags: c.Truth.Tags, ExpectedDecision: c.Truth.Decision})
	}
	return out
}

// Find 按编号取用例。
func (s *Server) Find(id string) (*schema.Case, bool) {
	for _, c := range s.Cases {
		if c.ID == id {
			return c, true
		}
	}
	return nil, false
}

// Start 启动一次审核，立即返回 session，事件通过 SSE 订阅。
func (s *Server) Start(c *schema.Case, opt audit.Options, explain string, disabled []string) *Session {
	sess := &Session{ID: fmt.Sprintf("S%d", time.Now().UnixNano()), CaseID: c.ID, Started: time.Now(),
		done: make(chan struct{}), listeners: map[chan audit.Event]bool{}}
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()

	go func() {
		defer close(sess.done)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		ex, closer, err := s.connectTools(ctx, c)
		if err != nil {
			sess.fail("连接 MCP 工具失败：" + err.Error())
			return
		}
		defer closer()

		rv := &audit.Reviewer{LLM: s.LLM(), Client: s.LLM(), Cfg: &s.Cfg, Disabled: map[string]bool{}}
		if s.ReplayDir != "" {
			rv.Replay = &llm.Replayer{Dir: s.ReplayDir}
			rv.CaseID = c.ID
		}
		if s.DemoSynthetic {
			// 合成放在最后一层：真实调用 > 真实录制 > 合成。
			rv.LLM = &llm.SyntheticLLM{Case: c, Synth: &llm.Synthetic{}, Fallback: rv.LLM}
		}
		for _, d := range disabled {
			rv.Disabled[d] = true
		}
		opt.Explain = explain
		opt.OnEvent = sess.publish
		eng := audit.New(s.RF)
		rep := audit.Run(eng, ctx, &c.Claim, c.InvoiceText, c.SupportText, ex, rv, opt)
		sess.finish(rep)
		// 记账：按调用方分桶，便于报警时定位到人。
		if s.Quota != nil {
			s.Quota.Record(sess.QuotaClient, rep.CostUSD, c.ID)
		}
	}()
	return sess
}

// publish 把事件推给所有订阅者，并留存供重放。
func (s *Session) publish(ev audit.Event) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	chans := make([]chan audit.Event, 0, len(s.listeners))
	for ch := range s.listeners {
		chans = append(chans, ch)
	}
	s.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- ev:
		default:
			// 订阅者来不及时丢弃单个事件，不阻塞审核主流程。
			// 重连时界面会拿到完整事件列表补齐。
		}
	}
}

func (s *Session) finish(rep *audit.Report) {
	s.mu.Lock()
	s.report = rep
	s.mu.Unlock()
	s.broadcastClose()
}

func (s *Session) fail(msg string) {
	s.mu.Lock()
	s.err = msg
	s.mu.Unlock()
	s.broadcastClose()
}

func (s *Session) broadcastClose() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.listeners {
		close(ch)
		delete(s.listeners, ch)
	}
}

// Snapshot 返回当前事件与结果，供界面刷新或重连。
func (s *Session) Snapshot() ([]audit.Event, *audit.Report, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	evs := make([]audit.Event, len(s.events))
	copy(evs, s.events)
	select {
	case <-s.done:
		return evs, s.report, s.err, true
	default:
		return evs, nil, "", false
	}
}

// subscribe 注册一个订阅者，返回通道与取消函数。
func (s *Session) subscribe() (chan audit.Event, func()) {
	ch := make(chan audit.Event, 256)
	s.mu.Lock()
	s.listeners[ch] = true
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		if _, ok := s.listeners[ch]; ok {
			delete(s.listeners, ch)
			close(ch)
		}
		s.mu.Unlock()
	}
}

// AddReview 记录人工复核结论，并把"通过"类结论写进查重库。
//
// 这是本项目的记忆机制：人纠正过的结论会改变下一次查重的结果。
// 因为会改变业务状态，所以要求：操作人必须是名单里的真实工号，且结论可撤销。
func (s *Server) AddReview(r ReviewOutcome, history *schema.HistRecord) (ReviewOutcome, error) {
	c, ok := s.Find(r.CaseID)
	if !ok {
		return ReviewOutcome{}, fmt.Errorf("用例不存在: %s", r.CaseID)
	}
	saved := s.reviews.Add(r)

	// 只有"通过"类结论才写查重库：驳回的结论代表这张票不该报，
	// 写进去反而会让它以后被判"已报销过"，语义是错的。
	if r.Decision == policy.Approve || r.Decision == policy.ApproveAdj {
		h := *history
		h.ClaimNo = c.Claim.ClaimNo + "-RV"
		s.reviews.PutHistory(stubHistory{
			ClaimNo: h.ClaimNo, InvoiceNo: h.InvoiceNo, ApplicantID: h.ApplicantID,
			SellerTaxID: h.SellerTaxID, IssueDate: h.IssueDate, AmountFen: h.AmountFen,
			Category: h.Category, Status: h.Status, SubmitDate: h.SubmitDate,
		})
		saved.AffectedHistory = HistoryRef{ClaimNo: h.ClaimNo, InvoiceNo: h.InvoiceNo, Applied: true}
		// 把回写标记记回记录本身，撤销时按它回滚。
		s.reviews.UpdateAffected(saved.ID, saved.AffectedHistory)
	}
	return saved, nil
}

// RevokeReview 撤销一条复核结论：标记撤销并从查重库撤掉对应记录。
func (s *Server) RevokeReview(id, by, byName, reason string) (ReviewOutcome, error) {
	rec, already, err := s.reviews.Revoke(id, by, byName, reason)
	if err != nil {
		return ReviewOutcome{}, err
	}
	if already {
		return rec, nil
	}
	// 回滚查重库。
	if rec.AffectedHistory.Applied && rec.AffectedHistory.ClaimNo != "" {
		s.reviews.DropHistory(rec.AffectedHistory.ClaimNo)
	}
	return rec, nil
}

// Reviews 返回全部复核结论（含已撤销），最近在前。
func (s *Server) Reviews() []ReviewOutcome { return s.reviews.List() }

// extra 返回回写记录（供工具层做查重）。
func (s *Server) extra() []schema.HistRecord {
	stubs := s.reviews.Histories()
	out := make([]schema.HistRecord, 0, len(stubs))
	for _, h := range stubs {
		out = append(out, schema.HistRecord{
			ClaimNo: h.ClaimNo, InvoiceNo: h.InvoiceNo, ApplicantID: h.ApplicantID,
			SellerTaxID: h.SellerTaxID, IssueDate: h.IssueDate, AmountFen: h.AmountFen,
			Category: h.Category, Status: h.Status, SubmitDate: h.SubmitDate,
		})
	}
	return out
}

// Stats 汇总评测所需数字：结论分布、平均耗时、成本。
func (s *Server) Stats() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	dec := map[string]int{}
	costs, n := 0.0, 0
	for _, sess := range s.sessions {
		_, rep, _, done := sess.Snapshot()
		if !done || rep == nil {
			continue
		}
		dec[rep.Result.Decision]++
		costs += rep.CostUSD
		n++
	}
	return map[string]any{"sessions": n, "decisions": dec, "cost_usd": costs}
}

// ---- 工具连接 ----

// connectTools 复用 cmd 里的连接逻辑：远程优先，否则进程内起 MCP 服务。
func (s *Server) connectTools(ctx context.Context, c *schema.Case) (audit.Explorer, func(), error) {
	return connect(ctx, s.MCPURL, s.MCPData, c, s.MCPToken, s.extra())
}

// LLM 返回模型调用客户端（进程内单例）。
func (s *Server) LLM() *llm.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.llmClient == nil {
		cfg := s.Cfg
		s.llmClient = llm.NewClient(&cfg)
	}
	return s.llmClient
}

// StaticDir 返回 web 资源目录，便于测试。
var StaticDir fs.FS

// jsonOut 写 JSON 响应。
func jsonOut(w http.ResponseWriter, v any, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// sortedKeys 便于稳定输出。
func sortedKeys(m map[string]int) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

var _ = strings.TrimSpace
