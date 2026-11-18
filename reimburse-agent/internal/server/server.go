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
	"reimburse/internal/task"
	"reimburse/internal/upload"
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
	// TaskID 非空表示这次审核属于某个任务（续审时）。
	TaskID string
	// OnFinish 在审核完成后回调，用于更新任务状态。
	OnFinish func(*audit.Report)
	// OnFail 在审核异常中断时回调，用于把任务从"审核中"放回可重试状态。
	// 没有它，一次失败就会让任务永远卡在"审核中"，谁也再动不了。
	OnFail func(string)

	mu       sync.Mutex
	events   []audit.Event
	report   *audit.Report
	err      string
	done     chan struct{}
	doneOnce sync.Once
	// listeners 是正在订阅的 SSE 连接。
	listeners map[chan audit.Event]bool
}

// Server 是 HTTP 服务。
type Server struct {
	reviewMu sync.Mutex
	Root     string // 数据根目录（含 cases/）
	Cases    []*schema.Case
	RF       *policy.File
	Cfg      llm.Config
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
	// Tasks 是任务生命周期库；为空时不启用任务流。
	Tasks *task.Store
	// llmClient 延迟创建，便于测试替换。
	llmClient *llm.Client
	// ReplayDir 不为空时，实时调用失败就回放录制；界面会标明"回放"。
	ReplayDir string
	// DemoSynthetic 为真时，连录制都没有就用合成抽取。只在无密钥演示时开启。
	DemoSynthetic bool
	// Uploader 处理真实票据上传；为空时不接受上传。
	Uploader *upload.Processor
	// LocalOperator 是本机（未启用令牌）访问时绑定的工号。
	//
	// 为什么需要：监听 127.0.0.1 时不走令牌鉴权，请求上下文里没有身份，
	// 复核就会被默认拒绝——本机自己演示都做不了。这里给一个明确的本机操作人，
	// 而不是"没身份也放行"。跨机部署必须配令牌，本字段不生效。
	// 空串表示本机也不绑定身份（此时本机复核同样被拒绝）。
	LocalOperator string
	// LocalDemoOperators lets loopback requests choose a reviewer for local rehearsals.
	LocalDemoOperators bool
}

// New 创建服务。
func New(root string, rf *policy.File, cfg llm.Config) (*Server, error) {
	cases, err := schema.LoadAll(root)
	if err != nil {
		return nil, err
	}
	uploads, err := upload.LoadSaved(root)
	if err != nil {
		return nil, fmt.Errorf("加载已上传单据: %w", err)
	}
	s := &Server{Root: root, Cases: append(cases, uploads...), RF: rf, Cfg: cfg,
		sessions: map[string]*Session{}, reviews: newReviewStore()}
	return s, nil
}

// Index 返回用例列表。
func (s *Server) Index() []CaseIndex {
	out := make([]CaseIndex, 0, len(s.Cases))
	for _, c := range s.Cases {
		title, tags, expected := c.Truth.Title, c.Truth.Tags, c.Truth.Decision
		if !c.HasTruth {
			// 上传的票据没有标准答案：标题要自己造，也不该有"期望结论"。
			title, tags, expected = uploadedTitle(c), []string{c.Claim.Category, "uploaded"}, ""
		}
		out = append(out, CaseIndex{ID: c.ID, Title: title, Category: c.Claim.Category,
			Tags: tags, ExpectedDecision: expected})
	}
	return out
}

// AddCase 把一条新单据加入内存，使其可被审核。
//
// 上传的单据走这里进来：它们不在启动时加载的测试集里，
// 但与测试集结构一致，所以审核、任务、查重全部原样复用。
// 并发安全：审核会并发读 s.Cases。
func (s *Server) AddCase(c *schema.Case) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, old := range s.Cases {
		if old.ID == c.ID {
			s.Cases[i] = c // 同编号覆盖（重复上传同一张）
			return
		}
	}
	s.Cases = append(s.Cases, c)
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

// StartTask 是续审入口：在已有任务上再跑一轮，并把 recheck 计划写进事件流。
//
// 与 Start 的区别只在于"起点"：Start 是全新单据，StartTask 带着上一轮的
// 补件清单与续审计划，审完会更新任务状态（轮次 +1）。
func (s *Server) StartTask(c *schema.Case, t *task.Task, opt audit.Options, explain string, disabled []string) *Session {
	return s.StartTaskAs(c, t, opt, explain, disabled, "")
}

// StartTaskAs 是续审入口，额外指定配额调用方。
func (s *Server) StartTaskAs(c *schema.Case, t *task.Task, opt audit.Options, explain string, disabled []string, quotaClient string) *Session {
	// 续审前先把"上一轮查了什么、这轮要重查什么"告诉读者。
	opt.Recheck = convertRecheck(t.NextRecheck)
	opt.ResumeHint = fmt.Sprintf("第 %d 轮续审：使用补交的材料重新执行完整审核（%d 份附件）",
		len(t.Runs)+1, len(t.Attachments))

	// 关键：把补交的材料真正喂进判定。
	// 否则"补交"只是登记了一个文件名，续审还是拿原始缺件重跑，结论不会变——
	// 那这个闭环就是假的。这里按补件类型把内容落到单据上（佐证凭证、报销单字段等）。
	eff, unknown := applySupplements(c, t)
	if len(t.Attachments) > 0 {
		opt.ResumeHint += fmt.Sprintf("；已把 %d 份补交材料写入单据", len(t.Attachments))
	}
	if len(unknown) > 0 {
		opt.ResumeHint += "；以下补交项无法对应到单据字段，未采用：" + strings.Join(unknown, "、")
	}

	// 参数在启动前就位：TaskID 与 OnFinish 一起传进去，不存在
	// "goroutine 先跑完、回调还没挂上"的窗口。
	return s.startWith(eff, opt, explain, disabled, SessionOpts{
		TaskID:      t.ID,
		QuotaClient: quotaClient,
		OnFinish: func(rep *audit.Report) {
			s.applyReportToTask(t.ID, rep, eff)
		},
		OnFail: func(string) { s.releaseTask(t.ID) },
	})
}

// SessionOpts 是一次审核启动前必须定好的全部外部参数。
//
// 为什么要把它们集中起来：之前 Start() 先起 goroutine，再让调用方设置
// TaskID / OnFinish / QuotaClient。审核跑得快时 goroutine 已经 finish 了，
// 那几个字段还是空的——任务状态不会更新（转人工后待办不消失）、
// 成本也记不到人头上。这类竞争不会报错，只会偶尔少记一笔，最难查。
// 现在要求：这些参数在启动之前必须齐备。
type SessionOpts struct {
	// TaskID 非空表示这次审核属于某个任务（续审或首次建任务）。
	TaskID string
	// OnFinish 在审核完成后调用，用于把结论落到任务状态上。
	OnFinish func(*audit.Report)
	// OnFail 在审核异常中断时调用，用于把任务从"审核中"放回可重试状态。
	// 没有它，一次失败就会让任务永远卡在"审核中"，谁也再动不了。
	OnFail func(string)
	// QuotaClient 是配额分桶用的调用方标识。
	QuotaClient string
}

// Start 启动一次审核，立即返回 session，事件通过 SSE 订阅。
func (s *Server) Start(c *schema.Case, opt audit.Options, explain string, disabled []string) *Session {
	return s.startWith(c, opt, explain, disabled, SessionOpts{})
}

// startWith 是唯一的启动入口：参数齐了才起 goroutine。
func (s *Server) startWith(c *schema.Case, opt audit.Options, explain string, disabled []string, so SessionOpts) *Session {
	sess := &Session{ID: fmt.Sprintf("S%d", time.Now().UnixNano()), CaseID: c.ID, Started: time.Now(),
		done: make(chan struct{}), listeners: map[chan audit.Event]bool{},
		TaskID: so.TaskID, OnFinish: so.OnFinish, OnFail: so.OnFail, QuotaClient: so.QuotaClient}
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()

	go func() {
		defer sess.markDone()
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
	if s.OnFinish != nil {
		s.OnFinish(rep)
	}
	// 顺序要紧：先标记完成，再通知订阅者通道关闭。
	// 反过来会出现一个窗口——订阅者收到"流结束"，去读快照，却被告知"还没完成"。
	s.markDone()
	s.broadcastClose()
}

// markDone 关闭 done 通道，表示本次审核已经结束。
// 必须在广播"订阅结束"之前调用：订阅方收到通道关闭后会立刻读一次快照，
// 如果那时 done 还没关闭，快照会返回"未完成且没有报告"——界面就会看到空结论。
func (s *Session) markDone() {
	s.doneOnce.Do(func() { close(s.done) })
}

func (s *Session) fail(msg string) {
	s.mu.Lock()
	s.err = msg
	onFail := s.OnFail
	s.mu.Unlock()
	if onFail != nil {
		onFail(msg)
	}
	s.markDone()
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

// OperatorOf 解析这次请求的操作人：跨机看令牌绑定，本机看 LocalOperator。
// 返回空串表示无法确定身份，写入类操作必须拒绝。
func (s *Server) OperatorOf(r *http.Request) string {
	if id := BoundEmployeeID(r); id != "" {
		return id
	}
	// 本机直连：RemoteAddr 就是回环地址，且本次请求没有令牌。
	if isLoopbackAddr(r.RemoteAddr) {
		return s.LocalOperator
	}
	return ""
}

// isLoopbackAddr 判断远端是否为回环地址。
func isLoopbackAddr(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

// OpenReviewStore 打开复核记录文件。path 为空表示只在内存（重启即失，仅测试用）。
//
// 线上必须落盘：复核结论会改变查重结果，重启后丢了，下一次同票就会被放行。
func (s *Server) OpenReviewStore(path string) error {
	if path == "" {
		return nil
	}
	return s.reviews.OpenPath(path)
}

// AddReview 记录人工复核结论，并把"通过"类结论写进查重库。
//
// 这是本项目的记忆机制：人纠正过的结论会改变下一次查重的结果。
// 因为会改变业务状态，所以要求：操作人必须是名单里的真实工号，且结论可撤销。
func (s *Server) AddReview(r ReviewOutcome, history *schema.HistRecord) (ReviewOutcome, error) {
	s.reviewMu.Lock()
	defer s.reviewMu.Unlock()
	c, ok := s.Find(r.CaseID)
	if !ok {
		return ReviewOutcome{}, fmt.Errorf("用例不存在: %s", r.CaseID)
	}
	if !isApproval(r.Decision) {
		if _, approved := s.activeApproval(r.CaseID); approved {
			return ReviewOutcome{}, fmt.Errorf("这张单已有生效的人工通过记录，请先撤销通过，再修改复核结论")
		}
	} else if s.Tasks != nil {
		if t, found := s.Tasks.FindByCase(r.CaseID); found && t.State == task.Withdrawn {
			return ReviewOutcome{}, fmt.Errorf("这张单的任务已撤销，不能确认通过")
		}
	}
	// 只有"通过"类结论才写查重库：驳回的结论代表这张票不该报，
	// 写进去反而会让它以后被判"已报销过"，语义是错的。
	var put *stubHistory
	if r.Decision == policy.Approve || r.Decision == policy.ApproveAdj {
		// 没有发票号就写不了查重：那会是一条查不出任何东西的空记录。
		// 真实票据在模型不可用时正是这种情况——此时应当明确拒绝，
		// 而不是假装记下了。宁可让财务知道"这张票没进查重库"。
		if history.InvoiceNo == "" {
			return ReviewOutcome{}, fmt.Errorf("无法取得发票号码，本条通过结论不能写入查重库；请先完成审核（或配置模型）后重试")
		}
		h := *history
		// 主键由 AddWithHistory 追加复核记录编号（见 review_store.go），
		// 这里只给基数，保证"同一张单的每次通过各有一条查重记录"。
		h.ClaimNo = c.Claim.ClaimNo + "-RV"
		sh := stubHistory{
			ClaimNo: h.ClaimNo, InvoiceNo: h.InvoiceNo, ApplicantID: h.ApplicantID,
			SellerTaxID: h.SellerTaxID, IssueDate: h.IssueDate, AmountFen: h.AmountFen,
			Category: h.Category, Status: h.Status, SubmitDate: h.SubmitDate,
		}
		put = &sh
	}

	// 记录与查重回写一次写完：分两次写，第二次失败会留下"记录在、查重没写"的半成功状态。
	saved, dup, err := s.reviews.AddWithHistory(r, put)
	if err != nil {
		return ReviewOutcome{}, err
	}
	saved.Duplicate = dup
	if isApproval(saved.Decision) {
		if err := s.syncTaskApproval(c); err != nil {
			return saved, fmt.Errorf("人工通过已保存，但任务状态未同步。可原样重试：%w", err)
		}
	}
	return saved, nil
}

// RevokeReview 撤销一条复核结论：标记撤销并从查重库撤掉对应记录。
func (s *Server) RevokeReview(id, by, byName, reason string) (ReviewOutcome, error) {
	s.reviewMu.Lock()
	defer s.reviewMu.Unlock()
	rec, _, err := s.reviews.Revoke(id, by, byName, reason)
	if err != nil {
		return ReviewOutcome{}, err
	}
	// 回滚查重库。落盘失败要报出去：内存里已标记撤销、盘上还有查重记录，
	// 那下一次同票仍会被判"报过"，而界面以为已经撤了。
	if rec.AffectedHistory.Applied && rec.AffectedHistory.ClaimNo != "" {
		if _, err := s.reviews.DropHistory(rec.AffectedHistory.ClaimNo); err != nil {
			return ReviewOutcome{}, err
		}
	}
	if isApproval(rec.Decision) && s.Tasks != nil {
		if c, ok := s.Find(rec.CaseID); ok {
			if _, found := s.activeApproval(rec.CaseID); found {
				return rec, s.syncTaskApproval(c)
			}
		}
		if t, ok := s.Tasks.FindByCase(rec.CaseID); ok && t.ApprovalReviewID == rec.ID {
			return rec, s.Tasks.ReopenAfterApprovalRevoked(t.ID, rec.ID, by)
		}
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
	s.reviewMu.Lock()
	own := map[string]bool{}
	for _, r := range s.Reviews() {
		if r.CaseID == c.ID && r.AffectedHistory.Applied {
			own[r.AffectedHistory.ClaimNo] = true
		}
	}
	var history []schema.HistRecord
	for _, h := range s.extra() {
		if !own[h.ClaimNo] {
			history = append(history, h)
		}
	}
	s.reviewMu.Unlock()
	return connect(ctx, s.MCPURL, s.MCPData, c, s.MCPToken, history)
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
