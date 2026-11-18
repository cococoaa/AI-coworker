package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"reimburse/internal/audit"
	"reimburse/internal/llm"
	"reimburse/internal/mcpauth"
	"reimburse/internal/schema"
	"reimburse/internal/task"
	"reimburse/plugins/reimburse-audit/policy"
)

// Routes 注册全部路由。
//
//	GET  /                     单页界面
//	GET  /api/cases            用例列表
//	GET  /api/cases/{id}       单张单据明细（报销单、发票原文、凭证原文）
//	POST /api/audit            启动一次审核，返回 session id
//	GET  /api/audit/{sid}/events  SSE 事件流（先补发已发生的事件，再实时推送）
//	GET  /api/audit/{sid}      会话快照（刷新页面用）
//	POST /api/review           记录人工复核结论
//	GET  /api/reviews          复核记录
//	GET  /api/meta             运行时信息：模型、规则、工具接入方式、健康状态
//	GET  /api/stats            本次运行累计统计
//	POST /api/upload           上传真实票据（multipart），返回可审核的单号
//	POST /api/upload/json      同上，JSON + base64（脚本用）
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/cases", s.handleCases)
	mux.HandleFunc("/api/cases/", s.handleCase)
	mux.HandleFunc("/api/audit", s.handleAudit)
	mux.HandleFunc("/api/audit/", s.handleAuditSub)
	mux.HandleFunc("/api/review", s.handleReview)
	mux.HandleFunc("/api/review/revoke", s.handleRevoke)
	mux.HandleFunc("/api/reviews", s.handleReviews)
	mux.HandleFunc("/api/quota", s.handleQuota)
	mux.HandleFunc("/api/tasks", s.handleTasks)
	mux.HandleFunc("/api/tasks/", s.handleTaskSub)
	mux.HandleFunc("/api/people", s.handlePeople)
	mux.HandleFunc("/api/meta", s.handleMeta)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/upload", s.handleUpload)
	mux.HandleFunc("/api/upload/json", s.handleUploadJSON)
	mux.HandleFunc("/api/sample-invoice", s.handleSampleInvoice)
	return logRequests(s.withOperator(mux))
}

// withOperator 把"本机操作人"注进请求上下文。
//
// 只在本机回环访问、且请求没带令牌时生效——这时走的是"本机直连"，
// 没有经过令牌鉴权，上下文里本来没有身份。跨机访问仍以令牌绑定为准。
func (s *Server) withOperator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.LocalOperator != "" && BoundEmployeeID(r) == "" && isLoopbackAddr(r.RemoteAddr) {
			r = r.WithContext(context.WithValue(r.Context(), localOpKey{}, s.LocalOperator))
		}
		next.ServeHTTP(w, r)
	})
}

// logRequests 记录每个请求，便于现场排查"点了没反应"。
//
// 同时兜底 recover：一个 handler 崩掉不应该让浏览器看到连接被重置
// （那个现象极难排查——服务还活着，只是这个请求没了）。
// 崩了就返回 500 并写清是哪个路径，让人知道该看哪里。
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Printf("panic %s %s: %v\n%s", r.Method, r.URL.Path, v, debug.Stack())
				http.Error(w, "服务内部错误："+fmt.Sprint(v), 500)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	page := PageHTML()
	if len(page) == 0 {
		http.Error(w, "界面资源缺失", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}

func (s *Server) handleCases(w http.ResponseWriter, r *http.Request) {
	jsonOut(w, map[string]any{"cases": s.Index()}, 200)
}

func (s *Server) handleCase(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/cases/")
	c, ok := s.Find(id)
	if !ok {
		jsonOut(w, map[string]any{"error": "用例不存在: " + id}, 404)
		return
	}
	out := map[string]any{
		"id": c.ID, "claim": c.Claim, "invoice": c.InvoiceText, "support": c.SupportText,
		"title": c.Truth.Title, "tags": c.Truth.Tags,
		// 上传的票据没有标准答案：标题与标签要自己造，不能显示空白。
		"uploaded": !c.HasTruth,
	}
	if !c.HasTruth {
		out["title"] = uploadedTitle(c)
		out["tags"] = []string{c.Claim.Category, "uploaded"}
	} else {
		// 标准答案默认不由前端展示，但提供给"对照"按钮使用。
		out["expected"] = map[string]any{"decision": c.Truth.Decision, "hits": c.Truth.Hits,
			"approved_fen": c.Truth.ApprovedFen}
	}
	jsonOut(w, out, 200)
}

// uploadedTitle 给上传的单据造一个可读标题。
// 没有标准答案就没有现成标题，但界面上不能是空白。
func uploadedTitle(c *schema.Case) string {
	cat := map[string]string{"transport": "交通", "lodging": "住宿", "meal": "餐饮"}[c.Claim.Category]
	if cat == "" {
		cat = c.Claim.Category
	}
	title := "上传票据 · " + cat
	if c.Claim.ClaimNo != "" {
		title += " · " + c.Claim.ClaimNo
	}
	return title
}

// auditRequest 是启动审核的请求体。所有开关随请求记录，事后可复现。
type auditRequest struct {
	CaseID   string   `json:"case_id"`
	Explain  string   `json:"explain"`  // "" / deepseek / claude
	Disabled []string `json:"disabled"` // 演示：把某模型设为不可用
	Faults   []struct {
		Tool string `json:"tool"` // verify_invoice / check_company / check_history
		Mode string `json:"mode"` // timeout / error / garbled
	} `json:"faults"`
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonOut(w, map[string]any{"error": "只支持 POST"}, 405)
		return
	}
	var req auditRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		jsonOut(w, map[string]any{"error": "请求体解析失败: " + err.Error()}, 400)
		return
	}
	c, ok := s.Find(req.CaseID)
	if !ok {
		jsonOut(w, map[string]any{"error": "用例不存在: " + req.CaseID}, 404)
		return
	}
	// 配额预检：预算耗尽时熔断，不再自动判定。
	//
	// 注意这里**不做降级**（不用更便宜的模型凑合判）。积压是可见的，错批是不可见的。
	if s.Quota != nil {
		client := quotaClient(r)
		st := s.Quota.Check(client)
		if st.Blocked {
			jsonOut(w, map[string]any{
				"error":  "本周 AI 用量已达上限，自动审核已熔断",
				"quota":  st,
				"action": "该单据需转人工处理；请等待下周预算刷新或由管理员调整预算",
			}, 429)
			return
		}
	}
	opt := audit.Options{Injected: map[string]audit.Status{}}
	for _, f := range req.Faults {
		switch f.Mode {
		case "timeout":
			opt.Injected[f.Tool] = audit.ToolTimeout
		case "error", "failed", "garbled":
			opt.Injected[f.Tool] = audit.ToolFailed
		default:
			jsonOut(w, map[string]any{"error": "未知故障方式: " + f.Mode}, 400)
			return
		}
	}
	// 有任务库时，首次审核也建任务——否则"退回补材料"之后就没有下文了。
	//
	// 但同一张单据只应有一个任务。先找再建：否则重跑一次审核就多出一个任务，
	// 待办列表里会出现同一张单的两三条记录，财务不知道点哪个。
	var taskID string
	if s.Tasks != nil {
		// 不管上一个任务是什么状态都要复用：同一张报销单只有一个任务，
		// 再次审核就是这张单的下一轮（终态的单再审，属于复核后重审）。
		if exist, ok := s.Tasks.FindByCase(c.ID); ok {
			taskID = exist.ID
		} else {
			tk, err := s.Tasks.Create(c.ID, c.Claim.ClaimNo, c.Claim.Applicant.EmployeeID)
			if err != nil {
				jsonOut(w, map[string]any{"error": err.Error()}, 400)
				return
			}
			taskID = tk.ID
		}
	}
	// 参数在启动前全部就位，避免审核太快导致任务状态或成本漏记。
	so := SessionOpts{TaskID: taskID, QuotaClient: quotaClient(r)}
	if taskID != "" {
		so.OnFinish = func(rep *audit.Report) { s.applyReportToTask(taskID, rep, c) }
	}
	sess := s.startWith(c, opt, req.Explain, req.Disabled, so)
	out := map[string]any{"session_id": sess.ID, "case_id": c.ID, "started": sess.Started}
	if taskID != "" {
		out["task_id"] = taskID
	}
	jsonOut(w, out, 200)
}

func (s *Server) handleAuditSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/audit/")
	parts := strings.SplitN(rest, "/", 2)
	sid := parts[0]
	s.mu.Lock()
	sess, ok := s.sessions[sid]
	s.mu.Unlock()
	if !ok {
		jsonOut(w, map[string]any{"error": "会话不存在: " + sid}, 404)
		return
	}
	if len(parts) == 2 && parts[1] == "events" {
		s.streamEvents(w, r, sess)
		return
	}
	evs, rep, errMsg, done := sess.Snapshot()
	jsonOut(w, map[string]any{"events": evs, "report": rep, "error": errMsg, "done": done}, 200)
}

// streamEvents 是 SSE 端点。
//
// 顺序：先发一个 snapshot 事件（已发生的事件 + 是否已结束），再实时推送后续事件，
// 最后发 done。这样即使浏览器连上时审核已经跑了一半，也能看到完整过程。
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, sess *Session) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "当前连接不支持流式输出", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)

	send := func(name string, v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	evs, rep, errMsg, done := sess.Snapshot()
	if !send("snapshot", map[string]any{"events": evs, "report": rep, "error": errMsg, "done": done}) {
		return
	}
	if done {
		if rep != nil {
			send("report", rep)
		}
		send("done", map[string]any{"ok": errMsg == ""})
		return
	}

	ch, cancel := sess.subscribe()
	defer cancel()
	// 心跳：长时间无事件时保活，避免中间代理断开。
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				// 审核结束：补发最终报告。
				_, rep, errMsg, _ := sess.Snapshot()
				if rep != nil {
					send("report", rep)
				}
				send("done", map[string]any{"ok": errMsg == ""})
				return
			}
			if !send("event", ev) {
				return
			}
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// reviewRequest 是提交复核结论的请求体。
// 关键变化：operator 不再是前端自称的字符串，而是必须与令牌一起使用的工号，
// 由服务端按名单校验（见 identity 包）。
type reviewRequest struct {
	CaseID   string `json:"case_id"`
	ClaimNo  string `json:"claim_no"`
	Decision string `json:"decision"`
	Note     string `json:"note"`
	// EmployeeID 是操作人工号。缺省时回退到 X-Employee-ID 请求头。
	EmployeeID string `json:"employee_id"`
	// DecisionBefore 是提交时的系统自动判定，用于对比"人是否改了结论"。
	DecisionBefore string `json:"decision_before"`
	// SessionID 是这次复核对应的审核会话。带上它，查重记录就能用
	// 本次真实抽出的发票号，而不是只靠标准答案（真实票据没有标准答案）。
	SessionID string `json:"session_id"`
}

// identify 取当前操作人的工号。
//
// 工号只能来自令牌绑定（令牌名即工号，一令牌一人）。
// 请求体里的 employee_id、请求头 X-Employee-ID 一律不再采信——
// 谁都能在 body 里写一个别人的工号，那样"复核可追溯到人"就是一句空话。
//
// 本机未启用鉴权（没有令牌）时返回空串，调用方据此拒绝写入（默认拒绝）。
// bodyID 参数保留只为兼容调用点，不再参与判断。
func identify(bodyID string, r *http.Request) string {
	_ = bodyID // 明确忽略：不接受调用方自报身份
	return operatorOfRequest(r)
}

// operatorOfRequest 从请求上下文里取操作人。
// 经 Server 的路由时用 Server.OperatorOf（含本机绑定）；否则只认令牌绑定。
func operatorOfRequest(r *http.Request) string {
	if id := BoundEmployeeID(r); id != "" {
		return id
	}
	if v := r.Context().Value(localOpKey{}); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// localOpKey 用于把本机操作人注入请求上下文。
type localOpKey struct{}

// BoundEmployeeID 返回令牌绑定的工号。
// 没有令牌（本机未启用鉴权）或令牌未绑定时返回空串。
func BoundEmployeeID(r *http.Request) string {
	ti := mcpauth.TokenInfoFrom(r.Context())
	if ti == nil {
		return ""
	}
	// UserID 就是令牌名；按约定令牌名直接用工号（如 E3001）。
	// 兼容 "E3001:王会计" 这种写法，只取冒号前。
	id := strings.TrimSpace(ti.UserID)
	if k := strings.Index(id, ":"); k > 0 {
		id = strings.TrimSpace(id[:k])
	}
	return id
}

func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonOut(w, map[string]any{"error": "只支持 POST"}, 405)
		return
	}
	var req reviewRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		jsonOut(w, map[string]any{"error": "请求体解析失败"}, 400)
		return
	}
	c, ok := s.Find(req.CaseID)
	if !ok {
		jsonOut(w, map[string]any{"error": "用例不存在"}, 404)
		return
	}
	switch req.Decision {
	case "approve", "approve_with_adjustment", "reject", "need_more_info", "manual_review":
	default:
		jsonOut(w, map[string]any{"error": "结论取值不合法"}, 400)
		return
	}

	// 身份校验：工号必须在名单里，且有复核权限。默认拒绝。
	id := identify(req.EmployeeID, r)
	if s.People == nil {
		jsonOut(w, map[string]any{"error": "服务未配置人员名单，拒绝写入复核结论"}, 403)
		return
	}
	person, err := s.People.Authenticate(id, "review")
	if err != nil {
		jsonOut(w, map[string]any{"error": err.Error()}, 403)
		return
	}

	rv := ReviewOutcome{
		CaseID: req.CaseID, ClaimNo: req.ClaimNo, Decision: req.Decision, Note: req.Note,
		Operator: person.EmployeeID, OperatorName: person.Name, DecisionBefore: req.DecisionBefore,
	}
	// 计算这条结论会写什么进查重库（只有"通过"类才写）。
	status := schema.HistRejected
	if req.Decision == policy.Approve || req.Decision == policy.ApproveAdj {
		status = schema.HistApproved
	}
	// 查重记录的发票信息：优先用本次审核真正抽出来的字段，没有才回落到标准答案。
	//
	// 真实票据没有 truth.json。如果这里只读 Truth，发票号会是空串，
	// 查重记录就成了一条没有主键的空壳——"这张票报过"根本查不出来，
	// 重复报销照样能过。宁可这条记录字段少一点，也不能是空的。
	inv := histFieldsFromReport(req.SessionID, req.CaseID, s)
	hist := &schema.HistRecord{
		InvoiceNo: inv.InvoiceNo, ApplicantID: c.Claim.Applicant.EmployeeID,
		SellerTaxID: inv.SellerTaxID, IssueDate: inv.IssueDate,
		AmountFen: inv.AmountFen, Category: c.Claim.Category,
		Status: status, SubmitDate: c.Claim.SubmitDate,
	}
	if hist.InvoiceNo == "" && c.HasTruth {
		hist.InvoiceNo = c.Truth.Invoice.InvoiceNo
		hist.SellerTaxID = c.Truth.Invoice.SellerTaxID
		hist.IssueDate = c.Truth.Invoice.IssueDate
		hist.AmountFen = c.Truth.Invoice.TotalFen
	}
	saved, err := s.AddReview(rv, hist)
	if err != nil {
		// 落盘失败要给 5xx：这不是用户输错，是服务没存下来。
		// 返回 400 会让人以为"改个参数就能重试"，实际要修的是磁盘。
		code := 400
		if isPersistErr(err) {
			code = 503
		}
		jsonOut(w, map[string]any{"ok": false, "error": err.Error(),
			"note": "本条结论未生效，可原样重试"}, code)
		return
	}
	// 复核结论必须回落到流程上：否则转人工的单永远挂在待办里，
	// 财务会以为没点成功，再点一次。
	out := map[string]any{
		"ok":        true,
		"review":    saved,
		"duplicate": saved.Duplicate,
		// 明确告诉前端这条结论对查重库做了什么。
		"impact": impactOf(saved),
	}
	if s.Tasks != nil {
		if tk, ok := s.Tasks.FindByCase(req.CaseID); ok {
			if updated, terr := s.Tasks.ResolveManual(tk.ID, req.Decision, person.EmployeeID, req.Note); terr == nil {
				out["task"] = updated
				out["task_state"] = updated.State
				out["waiting_on"] = updated.State.WaitingOn()
			} else {
				// 不是待人工复核的单：结论已记录，只是没改任务状态。如实告知。
				out["task_state"] = tk.State
				out["task_note"] = terr.Error()
			}
		}
	}
	jsonOut(w, out, 200)
}

// invFields 是查重需要的发票字段。
type invFields struct {
	InvoiceNo   string
	SellerTaxID string
	IssueDate   string
	AmountFen   int64
}

// histFieldsFromReport 从最近一次审核的抽取结果里取发票字段。
// 找不到会话（例如只做了复核没跑审核）时返回零值，由调用方决定回落策略。
func histFieldsFromReport(sessionID, caseID string, s *Server) invFields {
	var rep *audit.Report
	if sessionID != "" {
		s.mu.Lock()
		sess := s.sessions[sessionID]
		s.mu.Unlock()
		if sess != nil {
			if _, r, _, _ := sess.Snapshot(); r != nil {
				rep = r
			}
		}
	}
	if rep == nil {
		// 没带会话：取这张单最近一次审核的报告。
		if sess := s.latestSessionFor(caseID); sess != nil {
			if _, r, _, _ := sess.Snapshot(); r != nil {
				rep = r
			}
		}
	}
	if rep == nil || rep.Extract == nil {
		return invFields{}
	}
	get := func(name string) string {
		if f, ok := rep.Extract.Fields[name]; ok {
			return f.Value
		}
		return ""
	}
	out := invFields{
		InvoiceNo:   get(llm.FInvoiceNo),
		SellerTaxID: get(llm.FSellerTaxID),
		IssueDate:   get(llm.FIssueDate),
	}
	// 金额从"元"转"分"：抽取层统一用元、两位小数字符串。
	if v := get(llm.FTotalAmount); v != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			out.AmountFen = int64(f*100 + 0.5)
		}
	}
	return out
}

// latestSessionFor 找某张单据最近一次审核会话。
func (s *Server) latestSessionFor(caseID string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *Session
	for _, sess := range s.sessions {
		if sess.CaseID != caseID {
			continue
		}
		if best == nil || sess.Started.After(best.Started) {
			best = sess
		}
	}
	return best
}

// isPersistErr 判断错误是否来自落盘失败（需要 5xx，而不是 4xx）。
func isPersistErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, kw := range []string{"落盘失败", "落盘", "写入", "permission denied", "no space"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// quotaClient 返回调用方标识，用于配额分桶。
// 优先用令牌校验出的调用方名（mcpauth 中间件会写进 context），
// 拿不到时记 "anonymous"——不假装知道是谁。
func quotaClient(r *http.Request) string {
	if ti := mcpauth.TokenInfoFrom(r.Context()); ti != nil && ti.UserID != "" {
		return ti.UserID
	}
	// 本机未启用鉴权时没有令牌信息：记 "local"，不采信请求头里的自报工号。
	// 配额分桶可以粗，但不能假装知道是谁——那是审计的事，审计不接受自报。
	return "local"
}

// impactOf 把"这条结论会影响后续查重"讲清楚，供界面直接展示。
func impactOf(r ReviewOutcome) map[string]any {
	if !r.AffectedHistory.Applied {
		return map[string]any{
			"affects_dedup": false,
			"message":       "本条结论不写入查重库（只有通过/调整后通过才写），不影响后续单据。",
		}
	}
	return map[string]any{
		"affects_dedup": true,
		"history_key":   r.AffectedHistory.ClaimNo,
		"invoice_no":    r.AffectedHistory.InvoiceNo,
		"message": "本条结论已写入查重库：发票 " + r.AffectedHistory.InvoiceNo +
			" 将被标记为已报销过，后续同一张发票会被判重复报销。如需撤回，点「撤销」。",
	}
}

// revokeRequest 是撤销复核结论的请求体。
type revokeRequest struct {
	ID         string `json:"id"`
	Reason     string `json:"reason"`
	EmployeeID string `json:"employee_id"`
}

// handleRevoke 撤销一条复核结论，并从查重库回滚。
func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonOut(w, map[string]any{"error": "只支持 POST"}, 405)
		return
	}
	var req revokeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		jsonOut(w, map[string]any{"error": "请求体解析失败"}, 400)
		return
	}
	if req.ID == "" {
		jsonOut(w, map[string]any{"error": "缺少复核记录编号 id"}, 400)
		return
	}
	if s.People == nil {
		jsonOut(w, map[string]any{"error": "服务未配置人员名单，拒绝撤销"}, 403)
		return
	}
	person, err := s.People.Authenticate(identify(req.EmployeeID, r), "review")
	if err != nil {
		jsonOut(w, map[string]any{"error": err.Error()}, 403)
		return
	}
	reason := req.Reason
	if reason == "" {
		reason = "未填写原因"
	}
	rec, err := s.RevokeReview(req.ID, person.EmployeeID, person.Name, reason)
	if err != nil {
		code := 404
		if isPersistErr(err) {
			code = 503
		}
		jsonOut(w, map[string]any{"ok": false, "error": err.Error()}, code)
		return
	}
	jsonOut(w, map[string]any{
		"ok":     true,
		"review": rec,
		"impact": map[string]any{
			"affects_dedup": false,
			"message":       "已撤销，并从查重库撤掉对应记录；同一张发票不再因此被判重复报销。",
		},
	}, 200)
}

func (s *Server) handleReviews(w http.ResponseWriter, r *http.Request) {
	jsonOut(w, map[string]any{"reviews": s.Reviews()}, 200)
}

// handleTasks 是待办列表：财务最关心"今天有多少要处理、卡在谁那里"。
//
// 查询参数：
//
//	?state=need_manual  只看转人工的
//	?waiting=1          只看在别处的（待补材料 / 待人工）
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	if s.Tasks == nil {
		jsonOut(w, map[string]any{"enabled": false, "tasks": []any{}, "note": "未启用任务库"}, 200)
		return
	}
	all := s.Tasks.List()
	state := r.URL.Query().Get("state")
	waitingOnly := r.URL.Query().Get("waiting") == "1"

	// 初始化为空切片：nil 会序列化成 null，界面上 `tasks.map` 直接抛异常。
	out := make([]map[string]any, 0, len(all))
	now := time.Now()
	for _, t := range all {
		if state != "" && string(t.State) != state {
			continue
		}
		if waitingOnly && !(t.State == task.NeedMore || t.State == task.NeedManual) {
			continue
		}
		out = append(out, taskBrief(t, now))
	}
	jsonOut(w, map[string]any{
		"enabled": true, "tasks": out, "stats": s.Tasks.Stats(),
		"note": "waiting_on 说明这张单现在卡在谁那里；waiting_days 是卡了几天",
	}, 200)
}

// taskBrief 是列表项，只给必要字段。
func taskBrief(t *task.Task, now time.Time) map[string]any {
	out := map[string]any{
		"id": t.ID, "case_id": t.CaseID, "claim_no": t.ClaimNo,
		"state": t.State, "state_label": t.State.Label(),
		"waiting_on": t.State.WaitingOn(), "waiting_days": t.WaitingDays(now),
		"last_decision": t.LastDecision, "last_approved_fen": t.LastApprovedFen,
		"rounds": len(t.Runs), "updated_at": t.UpdatedAt,
	}
	if n := len(t.Outstanding()); n > 0 {
		out["outstanding"] = n
		var labels []string
		for _, m := range t.Outstanding() {
			labels = append(labels, m.Label)
		}
		out["outstanding_labels"] = labels
	}
	return out
}

// handleTaskSub 处理 /api/tasks/{id} 与 /api/tasks/{id}/resubmit。
func (s *Server) handleTaskSub(w http.ResponseWriter, r *http.Request) {
	if s.Tasks == nil {
		jsonOut(w, map[string]any{"error": "未启用任务库"}, 404)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]

	if len(parts) == 2 && parts[1] == "resubmit" {
		s.handleResubmit(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "reaudit" {
		s.handleReaudit(w, r, id)
		return
	}
	if r.Method != http.MethodGet {
		jsonOut(w, map[string]any{"error": "只支持 GET"}, 405)
		return
	}
	t, ok := s.Tasks.Get(id)
	if !ok {
		jsonOut(w, map[string]any{"error": "任务不存在: " + id}, 404)
		return
	}
	jsonOut(w, map[string]any{"task": t, "waiting_on": t.State.WaitingOn()}, 200)
}

// resubmitRequest 是补交材料的请求体。
type resubmitRequest struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Note string `json:"note"`
	// Content 是补交材料的正文。首版用文本；真实场景应是 PDF/图片上传 + OCR。
	// 必须有内容，否则"补交"只是登记，续审时判定不会变——那闭环就是假的。
	Content    string `json:"content"`
	EmployeeID string `json:"employee_id"`
}

// handleResubmit 登记补交。补交齐了任务回到"待审核"，等续审。
func (s *Server) handleResubmit(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		jsonOut(w, map[string]any{"error": "只支持 POST"}, 405)
		return
	}
	var req resubmitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		jsonOut(w, map[string]any{"error": "请求体解析失败"}, 400)
		return
	}
	by := identify(req.EmployeeID, r)
	if strings.TrimSpace(req.Content) == "" {
		jsonOut(w, map[string]any{
			"error": "补交材料缺少正文（content）。只登记文件名而不给内容，续审时判定不会变化。",
		}, 400)
		return
	}
	t, dup, err := s.Tasks.ResubmitIdem(id, req.Kind, req.Name, by, req.Note, req.Content)
	if err != nil {
		jsonOut(w, map[string]any{"error": err.Error()}, 400)
		return
	}
	out := map[string]any{
		"ok": true, "task": t, "duplicate": dup,
		"next": nextStep(t),
	}
	if dup {
		out["note"] = "同一份材料已提交过，本次没有重复登记（版本不变）"
	}
	jsonOut(w, out, 200)
}

// handleReaudit 续审：对已补齐的任务再跑一次审核。
//
// 与首次审核的区别：会带上 recheck 计划，让事件流里能看出"哪些是复用的、哪些是重查的"。
func (s *Server) handleReaudit(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		jsonOut(w, map[string]any{"error": "只支持 POST"}, 405)
		return
	}
	t, ok := s.Tasks.Get(id)
	if !ok {
		jsonOut(w, map[string]any{"error": "任务不存在: " + id}, 404)
		return
	}
	if t.State != task.Pending && t.State != task.NeedMore {
		jsonOut(w, map[string]any{
			"error": fmt.Sprintf("当前状态「%s」不能续审（只有待审核或待补材料可以）", t.State.Label()),
		}, 400)
		return
	}
	if !t.ResolvedAll() {
		jsonOut(w, map[string]any{
			"error":       "材料还没补齐，不能续审",
			"outstanding": t.Outstanding(),
		}, 400)
		return
	}
	// 抢占任务：连续点两次续审时，只有第一次能切开这个状态。
	// 放在建会话之前——先抢再跑，否则两次请求都会读到"待审核"。
	if _, err := s.Tasks.BeginAudit(t.ID); err != nil {
		jsonOut(w, map[string]any{"error": err.Error(), "task_id": t.ID}, 409)
		return
	}
	c, ok := s.Find(t.CaseID)
	if !ok {
		jsonOut(w, map[string]any{"error": "关联单据不存在: " + t.CaseID}, 404)
		return
	}
	// 复用首次审核的配额与工具链路。
	if s.Quota != nil {
		st := s.Quota.Check(quotaClient(r))
		if st.Blocked {
			jsonOut(w, map[string]any{
				"error": "本周 AI 用量已达上限，自动审核已熔断", "quota": st,
				"action": "该单据需转人工处理",
			}, 429)
			return
		}
	}
	sess := s.StartTaskAs(c, t, audit.Options{Injected: map[string]audit.Status{}}, "", nil, quotaClient(r))
	jsonOut(w, map[string]any{
		"session_id": sess.ID, "task_id": t.ID, "round": len(t.Runs) + 1,
		"recheck": t.NextRecheck,
	}, 200)
}

// nextStep 告诉调用方"接下来该做什么"。这是"能推动下一步"的直接体现。
func nextStep(t *task.Task) map[string]any {
	switch t.State {
	case task.NeedMore:
		var keys, labels []string
		for _, m := range t.Outstanding() {
			keys = append(keys, m.Key)
			labels = append(labels, m.Label)
		}
		return map[string]any{
			"action": "wait_for_employee",
			"who":    "报销人",
			"message": fmt.Sprintf("还需补交 %d 项：%s。补交后点「续审」重新审核，已核验的结果会复用。",
				len(keys), strings.Join(labels, "、")),
			"remaining": keys,
		}
	case task.Pending:
		if len(t.Runs) > 0 {
			return map[string]any{
				"action": "reaudit", "who": "系统",
				"message": "材料已补齐，可以续审。续审会复用以核验结果，只重查受影响的判定项。",
			}
		}
		return map[string]any{"action": "audit", "who": "系统", "message": "尚未审核，可以开始审核。"}
	case task.NeedManual:
		return map[string]any{"action": "wait_for_finance", "who": "财务复核",
			"message": "已转人工复核，等待财务确认。"}
	case task.Approved:
		return map[string]any{"action": "done", "who": "", "message": "初审通过，流程结束。"}
	case task.Rejected:
		return map[string]any{"action": "done", "who": "", "message": "已驳回，流程结束。"}
	}
	return map[string]any{"action": "none", "who": "", "message": "无后续动作。"}
}

// handleQuota 返回当前周用量。界面顶部与告警区用。
func (s *Server) handleQuota(w http.ResponseWriter, r *http.Request) {
	if s.Quota == nil {
		jsonOut(w, map[string]any{"enabled": false, "message": "未启用预算控制"}, 200)
		return
	}
	st := s.Quota.Check(quotaClient(r))
	cfg := s.Quota.Config()
	jsonOut(w, map[string]any{
		"enabled": true, "status": st,
		"config": map[string]any{
			"weekly_budget_usd":  cfg.WeeklyBudgetUSD,
			"warn_ratio":         cfg.WarnRatio,
			"critical_ratio":     cfg.CriticalRatio,
			"per_case_alert_usd": cfg.PerCaseAlertUSD,
		},
	}, 200)
}

// handlePeople 返回可选的复核人名单（只给工号与姓名，不给任何权限细节以外的信息）。
// 界面用它在登录时给出候选，避免手输工号出错。
func (s *Server) handlePeople(w http.ResponseWriter, r *http.Request) {
	if s.People == nil {
		jsonOut(w, map[string]any{"people": []any{}}, 200)
		return
	}
	type brief struct {
		EmployeeID string `json:"employee_id"`
		Name       string `json:"name"`
		Dept       string `json:"dept,omitempty"`
		CanReview  bool   `json:"can_review"`
		CanAudit   bool   `json:"can_audit"`
	}
	out := make([]brief, 0, 8)
	for _, p := range s.People.All() {
		if p.Disabled {
			continue
		}
		out = append(out, brief{p.EmployeeID, p.Name, p.Dept, p.CanReview, p.CanAudit})
	}
	jsonOut(w, map[string]any{"people": out}, 200)
}

// handleMeta 汇总界面顶部要显示的信息：模型、密钥是否就绪、工具接入方式、规则版本。
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	type modelInfo struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		KeyReady bool   `json:"key_ready"`
		Price    string `json:"price"`
	}
	dsReady := s.Cfg.DeepSeek.APIKey() != ""
	clReady := s.Cfg.Claude.APIKey() != ""
	jsonOut(w, map[string]any{
		"models": []modelInfo{
			{"deepseek", s.Cfg.DeepSeek.Model, dsReady, fmt.Sprintf("$%.2f/$%.2f 每百万 token", s.Cfg.DeepSeek.PriceIn, s.Cfg.DeepSeek.PriceOut)},
			{"claude", s.Cfg.Claude.Model, clReady, fmt.Sprintf("$%.2f/$%.2f 每百万 token", s.Cfg.Claude.PriceIn, s.Cfg.Claude.PriceOut)},
		},
		"rules": map[string]any{"policy": s.RF.Policy.Name, "version": s.RF.Policy.Version,
			"count": len(s.RF.Rules), "checks": len(s.RF.AllCheckIDs())},
		"tools": map[string]any{"mode": toolsMode(s.MCPURL), "url": s.MCPURL,
			"names": []string{"verify_invoice", "check_company", "check_history"}},
		"cases": len(s.Cases),
		// 真实性分两件事，不能混为一谈：模型是不是真在跑，核验数据是不是真业务接口。
		"provenance": map[string]any{
			"model_live":   dsReady || clReady,
			"tool_backend": "mock",
			"invoice_docs": "synthetic",
			"note":         "模型调用真实；票据为合成数据；三个核验工具读的是本地 mock 库，不是真实税局或工商接口",
		},
	}, 200)
}

func toolsMode(url string) string {
	if url == "" {
		return "in-process"
	}
	return "remote"
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st := s.Stats()
	// 结论分布按规则表优先级排序，避免前端自己排。
	dec, _ := st["decisions"].(map[string]int)
	ordered := make([]string, 0, len(dec))
	for _, d := range s.RF.Params.DecisionPriority {
		if _, ok := dec[d]; ok {
			ordered = append(ordered, d)
		}
	}
	st["order"] = ordered
	jsonOut(w, st, 200)
}
