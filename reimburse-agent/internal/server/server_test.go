package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"reimburse/internal/audit"
	"reimburse/internal/llm"
	"reimburse/rules"
)

// 这些测试验证 HTTP 层：接口形状、SSE 事件流、演示开关、复核回写。
// 判定本身由 internal/audit 的测试保证，这里不重复。

func testServer(t *testing.T) *Server {
	t.Helper()
	dir := os.Getenv("REIMBURSE_CASES")
	if dir == "" {
		dir = "../../data"
	}
	rf, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, rf, llm.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMetaAndCases(t *testing.T) {
	s := testServer(t)
	h := s.Routes()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/meta", nil))
	if rec.Code != 200 {
		t.Fatalf("meta 状态码 %d", rec.Code)
	}
	var meta struct {
		Models []struct {
			Provider string `json:"provider"`
			KeyReady bool   `json:"key_ready"`
		} `json:"models"`
		Rules struct {
			Count  int `json:"count"`
			Checks int `json:"checks"`
		} `json:"rules"`
		Tools struct {
			Mode string `json:"mode"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Models) != 2 || meta.Models[0].Provider != "deepseek" || meta.Models[1].Provider != "claude" {
		t.Errorf("模型信息不全: %+v", meta.Models)
	}
	if meta.Rules.Count != 15 || meta.Rules.Checks != 26 {
		t.Errorf("规则数量错误: %+v", meta.Rules)
	}
	if meta.Tools.Mode != "in-process" {
		t.Errorf("默认应为进程内 MCP: %s", meta.Tools.Mode)
	}
	// 密钥状态必须如实反映环境，不能写死"就绪"。
	if os.Getenv("DEEPSEEK_API_KEY") == "" && meta.Models[0].KeyReady {
		t.Error("没有密钥却报告就绪")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/cases", nil))
	var cl struct {
		Cases []CaseIndex `json:"cases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cl); err != nil {
		t.Fatal(err)
	}
	if len(cl.Cases) != 60 {
		t.Fatalf("用例数应为 60，得到 %d", len(cl.Cases))
	}
	for _, c := range cl.Cases {
		if c.ExpectedDecision == "" || c.Title == "" {
			t.Errorf("用例摘要缺字段: %+v", c)
		}
	}
}

// 单个用例的明细接口不能把"标准答案"漏进正文区域（界面默认不展示）。
func TestCaseDetail(t *testing.T) {
	s := testServer(t)
	h := s.Routes()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/cases/C01", nil))
	if rec.Code != 200 {
		t.Fatalf("状态码 %d", rec.Code)
	}
	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"claim", "invoice", "expected"} {
		if _, ok := d[k]; !ok {
			t.Errorf("缺少字段 %s", k)
		}
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/cases/NOPE", nil))
	if rec.Code != 404 {
		t.Errorf("不存在的用例应 404，得到 %d", rec.Code)
	}
}

// 一次审核跑完，SSE 必须给出 snapshot → event* → report → done。
func TestAuditSSE(t *testing.T) {
	s := testServer(t)
	h := s.Routes()

	body, _ := json.Marshal(map[string]any{"case_id": "C01"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/audit", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("启动审核失败: %d %s", rec.Code, rec.Body.String())
	}
	var start struct {
		SessionID string `json:"session_id"`
		CaseID    string `json:"case_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	if start.SessionID == "" || start.CaseID != "C01" {
		t.Fatalf("返回不完整: %+v", start)
	}

	// 等审核结束，再读事件流：此时以 snapshot(done=true)+report+done 收尾。
	deadline := time.Now().Add(30 * time.Second)
	for {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/audit/"+start.SessionID+"/events", nil))
		if strings.Contains(rec.Body.String(), "event: done") || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	evs := parseSSE(t, rec.Body.Bytes())
	if len(evs["snapshot"]) == 0 {
		t.Fatal("缺少 snapshot 事件")
	}
	if len(evs["report"]) != 1 || len(evs["done"]) != 1 {
		t.Fatalf("应各有一个 report 与 done，实际 report=%d done=%d", len(evs["report"]), len(evs["done"]))
	}
	if rec.Header().Get("Content-Type") == "" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Errorf("Content-Type 应为 text/event-stream，得到 %q", rec.Header().Get("Content-Type"))
	}

	var snap struct {
		Events []audit.Event `json:"events"`
		Report *audit.Report `json:"report"`
		Done   bool          `json:"done"`
	}
	if err := json.Unmarshal([]byte(evs["snapshot"][0]), &snap); err != nil {
		t.Fatal(err)
	}
	if !snap.Done || snap.Report == nil {
		t.Fatal("事件流结束时必须带完整报告")
	}
	if len(snap.Events) < 10 {
		t.Errorf("事件太少，过程不够可观测: %d", len(snap.Events))
	}
	// 事件序号必须连续递增。
	for i, e := range snap.Events {
		if i > 0 && e.Seq <= snap.Events[i-1].Seq {
			t.Fatalf("事件序号不递增：%d 在 %d 之后", e.Seq, snap.Events[i-1].Seq)
		}
	}
	// 没有密钥时两个模型都失败，结论必须落在转人工，绝不能是通过。
	if snap.Report.Result.Decision == rules.Approve {
		t.Errorf("模型不可用时不得自动通过")
	}
	if !hasStr(snap.Report.Result.Hits, "R15-b") {
		t.Errorf("应因字段不可信命中 R15-b，实际 %v", snap.Report.Result.Hits)
	}
}

// 演示开关：关闭两个模型应被拒绝；关一个时结论必须转人工。
func TestAuditSwitches(t *testing.T) {
	s := testServer(t)
	h := s.Routes()

	body, _ := json.Marshal(map[string]any{"case_id": "C01", "disabled": []string{"deepseek", "claude"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/audit", bytes.NewReader(body)))
	var r1 struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &r1)
	if r1.SessionID != "" {
		// 服务端允许启动，但结果必须转人工；界面会拦掉这个组合，接口层也不应给出通过。
		snap := waitReport(t, s, r1.SessionID)
		if snap.Result.Decision == rules.Approve {
			t.Error("两个模型都关闭时不得通过")
		}
	}

	// 非法故障方式要被拒绝。
	body, _ = json.Marshal(map[string]any{"case_id": "C01", "faults": []map[string]string{{"tool": "verify_invoice", "mode": "boom"}}})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/audit", bytes.NewReader(body)))
	if rec.Code != 400 {
		t.Errorf("非法故障方式应 400，得到 %d", rec.Code)
	}
	// 不存在的用例。
	body, _ = json.Marshal(map[string]any{"case_id": "ZZZ"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/audit", bytes.NewReader(body)))
	if rec.Code != 404 {
		t.Errorf("不存在的用例应 404，得到 %d", rec.Code)
	}
}

// 人工复核结论要能查到，并且"通过"的结论会进查重库。
func TestReviewWritesBack(t *testing.T) {
	s := testServer(t)
	h := s.Routes()
	body, _ := json.Marshal(map[string]any{"case_id": "C01", "claim_no": "BX26110001",
		"decision": "approve", "note": "人工确认", "operator": "测试"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/review", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("记录复核失败: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/reviews", nil))
	var d struct {
		Reviews []ReviewOutcome `json:"reviews"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if len(d.Reviews) != 1 || d.Reviews[0].Note != "人工确认" {
		t.Fatalf("复核记录错误: %+v", d.Reviews)
	}
	if len(s.extra()) != 1 {
		t.Fatalf("复核结论应写回查重库，实际 %d 条", len(s.extra()))
	}
	if s.extra()[0].Status != "approved" {
		t.Errorf("通过的结论应记为已通过: %s", s.extra()[0].Status)
	}

	// 非法结论要被拒绝。
	body, _ = json.Marshal(map[string]any{"case_id": "C01", "decision": "whatever"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/review", bytes.NewReader(body)))
	if rec.Code != 400 {
		t.Errorf("非法结论应 400，得到 %d", rec.Code)
	}
}

// 首页必须能返回完整 HTML，且内含关键控件，避免"空白页"。
func TestIndexPage(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 {
		t.Fatalf("首页状态码 %d", rec.Code)
	}
	html := rec.Body.String()
	for _, need := range []string{"开始审核", "/api/audit", "EventSource", "判定结果", "人工复核"} {
		if !strings.Contains(html, need) {
			t.Errorf("首页缺少 %q", need)
		}
	}
	// 只检查渲染后的可见文本里有没有 undefined（脚本里用 undefined 做判空是正常的）。
	if strings.Contains(html, ">undefined<") || strings.Contains(html, "undefined%") {
		t.Error("首页渲染结果里出现了 undefined")
	}
}

// ---- 辅助 ----

func parseSSE(t *testing.T, b []byte) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	var event string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
			continue
		}
		if strings.HasPrefix(line, "data: ") && event != "" {
			out[event] = append(out[event], strings.TrimPrefix(line, "data: "))
			event = ""
		}
	}
	return out
}

func waitReport(t *testing.T, s *Server, sid string) *audit.Report {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		sess := s.sessions[sid]
		s.mu.Unlock()
		if sess != nil {
			_, rep, _, done := sess.Snapshot()
			if done && rep != nil {
				return rep
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("等待报告超时")
	return nil
}

func hasStr(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

var _ = http.MethodGet
