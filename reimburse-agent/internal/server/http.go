package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"reimburse/internal/audit"
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
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/cases", s.handleCases)
	mux.HandleFunc("/api/cases/", s.handleCase)
	mux.HandleFunc("/api/audit", s.handleAudit)
	mux.HandleFunc("/api/audit/", s.handleAuditSub)
	mux.HandleFunc("/api/review", s.handleReview)
	mux.HandleFunc("/api/reviews", s.handleReviews)
	mux.HandleFunc("/api/meta", s.handleMeta)
	mux.HandleFunc("/api/stats", s.handleStats)
	return logRequests(mux)
}

// logRequests 记录每个请求，便于现场排查"点了没反应"。
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	jsonOut(w, map[string]any{
		"id": c.ID, "claim": c.Claim, "invoice": c.InvoiceText, "support": c.SupportText,
		"title": c.Truth.Title, "tags": c.Truth.Tags,
		// 标准答案默认不由前端展示，但提供给"对照"按钮使用。
		"expected": map[string]any{"decision": c.Truth.Decision, "hits": c.Truth.Hits,
			"approved_fen": c.Truth.ApprovedFen},
	}, 200)
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
	opt := audit.Options{Injected: map[string]audit.ToolStatus{}}
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
	sess := s.Start(c, opt, req.Explain, req.Disabled)
	jsonOut(w, map[string]any{"session_id": sess.ID, "case_id": c.ID, "started": sess.Started}, 200)
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

func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonOut(w, map[string]any{"error": "只支持 POST"}, 405)
		return
	}
	var rv ReviewOutcome
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&rv); err != nil {
		jsonOut(w, map[string]any{"error": "请求体解析失败"}, 400)
		return
	}
	if _, ok := s.Find(rv.CaseID); !ok {
		jsonOut(w, map[string]any{"error": "用例不存在"}, 404)
		return
	}
	switch rv.Decision {
	case "approve", "approve_with_adjustment", "reject", "need_more_info", "manual_review":
	default:
		jsonOut(w, map[string]any{"error": "结论取值不合法"}, 400)
		return
	}
	rv.At = time.Now()
	if rv.Operator == "" {
		rv.Operator = "财务复核"
	}
	s.AddReview(rv)
	jsonOut(w, map[string]any{"ok": true, "review": rv}, 200)
}

func (s *Server) handleReviews(w http.ResponseWriter, r *http.Request) {
	jsonOut(w, map[string]any{"reviews": s.Reviews()}, 200)
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
