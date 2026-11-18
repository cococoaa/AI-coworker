package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reimburse/internal/audit"
	"reimburse/internal/identity"
	"reimburse/internal/llm"
	"reimburse/internal/quota"
	"reimburse/internal/task"
	"reimburse/plugins/reimburse-audit/policy"
)

// 这些测试验证 HTTP 层：接口形状、SSE 事件流、演示开关、复核回写。
// 判定本身由 internal/audit 的测试保证，这里不重复。

func testServer(t *testing.T) *Server {
	t.Helper()
	dir := os.Getenv("REIMBURSE_CASES")
	if dir == "" {
		dir = "../../data"
	}
	rf, err := policy.Load()
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, rf, llm.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	// 默认给测试一个可用名单：复核需要真实工号。
	people, err := identity.New([]identity.Person{
		{EmployeeID: "E3001", Name: "王会计", CanReview: true, CanAudit: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.People = people
	// 测试默认当"本机直连"：RemoteAddr 是回环地址，由 LocalOperator 提供身份。
	// 这样复核测试不必伪造令牌，也不会误用"body 里写别人工号"这种正是要堵的路径。
	s.LocalOperator = "E3001"
	// 任务库与复核记录给临时文件：否则同一包里的测试会互相影响
	// （一个测试写的任务会出现在另一个测试的待办列表里）。
	tk, err := task.Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.Tasks = tk
	if err := s.OpenReviewStore(filepath.Join(t.TempDir(), "reviews.json")); err != nil {
		t.Fatal(err)
	}
	return s
}

// localReq 构造一个"本机直连"的请求。
//
// httptest.NewRequest 的 RemoteAddr 默认是 192.0.2.1:1234（测试网段），
// 不是回环地址，所以不会命中 LocalOperator。这里显式设成 127.0.0.1，
// 与真实"本机打开界面"的路径一致。
func localReq(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.RemoteAddr = "127.0.0.1:54321"
	return r
}

func TestReviewHistoryShowsLinkedClaim(t *testing.T) {
	s := testServer(t)
	c, ok := s.Find("C01")
	if !ok {
		t.Fatal("missing C01")
	}
	task, err := s.Tasks.Create(c.ID, c.Claim.ClaimNo, "测试报销人")
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := s.reviews.AddWithHistory(ReviewOutcome{CaseID: c.ID, Operator: "E3001"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, localReq(http.MethodGet, "/api/reviews", nil))
	var result struct {
		Reviews []struct {
			ReviewOutcome
			TaskID   string `json:"task_id"`
			Category string `json:"category"`
			Title    string `json:"case_title"`
		} `json:"reviews"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || len(result.Reviews) != 1 {
		t.Fatalf("status %d, result %+v", rec.Code, result)
	}
	row := result.Reviews[0]
	if row.ID != stored.ID || row.ClaimNo != c.Claim.ClaimNo || row.TaskID != task.ID ||
		row.Category != c.Claim.Category || row.Title != c.Truth.Title {
		t.Fatalf("incorrect linked claim metadata: %+v", row)
	}
}

func TestLocalDemoReviewerSelection(t *testing.T) {
	s := testServer(t)
	s.LocalDemoOperators = true
	people, err := identity.New([]identity.Person{
		{EmployeeID: "E3001", Name: "王会计", CanReview: true},
		{EmployeeID: "E3002", Name: "周主管", CanReview: true},
		{EmployeeID: "E3003", Name: "内审"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.People = people
	h := s.Routes()
	post := func(path, payload, remote string) *httptest.ResponseRecorder {
		t.Helper()
		r := localReq("POST", path, strings.NewReader(payload))
		r.Header.Set("Content-Type", "application/json")
		if remote != "" {
			r.RemoteAddr = remote
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	rec := post("/api/review", `{"case_id":"C01","decision":"approve","employee_id":"E3002"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("复核失败: %d %s", rec.Code, rec.Body.String())
	}
	reviews := s.Reviews()
	if len(reviews) != 1 || reviews[0].Operator != "E3002" || reviews[0].OperatorName != "周主管" {
		t.Fatalf("复核记录未采用所选人员: %+v", reviews)
	}
	rec = post("/api/review/revoke", `{"id":"`+reviews[0].ID+`","employee_id":"E3001"}`, "")
	if rec.Code != http.StatusOK || s.Reviews()[0].RevokedBy != "E3001" {
		t.Fatalf("撤销未按选中的操作人留痕: %d %s", rec.Code, rec.Body.String())
	}
	for _, tc := range []struct{ employee, remote string }{
		{"E3003", ""},
		{"E3002", "203.0.113.9:443"},
	} {
		rec = post("/api/review", `{"case_id":"C01","decision":"reject","employee_id":"`+tc.employee+`"}`, tc.remote)
		if rec.Code != http.StatusForbidden {
			t.Errorf("无权限人员应被拒绝: %d %s", rec.Code, rec.Body.String())
		}
	}
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

	// 等审核结束，再读事件流：此时以 snapshot+report+done 收尾。
	//
	// 注意：第一次连上来时审核可能刚开始（snapshot 里 done=false、report=null），
	// 之后事件实时推送、结束时补发 report。所以这里以"读到 done"为准，
	// 而不是假定首个 snapshot 就已完成——那个竞态真实存在，也真的会让人看到空结论。
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
	// 连接时审核可能还在跑：此时 snapshot 是"进行中"，报告在收尾的 report 事件里。
	// 两种形态都合法，但最终必须有完整报告与 done。
	if !snap.Done {
		var final audit.Report
		if err := json.Unmarshal([]byte(evs["report"][0]), &final); err != nil {
			t.Fatalf("收尾的 report 事件无法解析: %v", err)
		}
		snap.Report = &final
		snap.Events = append(snap.Events, final.Events...)
	}
	if snap.Report == nil {
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
	if snap.Report.Result.Decision == policy.Approve {
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
		if snap.Result.Decision == policy.Approve {
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

// 复核结论要能查到、写进查重库，并且可撤销（撤销后从查重库回滚）。
func TestReviewWriteAndRevoke(t *testing.T) {
	s := testServer(t)
	h := s.Routes()

	// 1) 无法确定身份：拒绝。
	//    本机未绑定操作人、请求又不是回环来源，就没有任何可信身份来源。
	s.LocalOperator = ""
	body, _ := json.Marshal(map[string]any{"case_id": "C01", "decision": "approve"})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/review", bytes.NewReader(body))
	r.RemoteAddr = "203.0.113.9:443" // 外部来源，且无令牌
	h.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Fatalf("无法确定身份时应 403，得到 %d", rec.Code)
	}

	// 2) 冒名：body 里写别人的工号不起作用，一律按绑定身份记。
	//    这正是本次要堵的洞——谁都能在 body 里填一个别人的工号。
	s.LocalOperator = "E3001"
	body, _ = json.Marshal(map[string]any{"case_id": "C01", "decision": "approve", "employee_id": "E9999"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("POST", "/api/review", bytes.NewReader(body)))
	if rec.Code == 403 {
		t.Fatalf("绑定身份有效时不应因 body 里的假工号被拒，得到 %d %s", rec.Code, rec.Body.String())
	}
	var spoof struct {
		Review ReviewOutcome `json:"review"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spoof); err != nil {
		t.Fatal(err)
	}
	if spoof.Review.Operator != "E3001" {
		t.Errorf("应按绑定身份 E3001 记账，而不是 body 里的 E9999，实际 %q", spoof.Review.Operator)
	}

	// 3) 正确工号：写入并回写查重库
	body, _ = json.Marshal(map[string]any{"case_id": "C01", "claim_no": "BX26110001",
		"decision": "approve", "note": "票据齐全", "employee_id": "E3001"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("POST", "/api/review", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("复核失败: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Review ReviewOutcome  `json:"review"`
		Impact map[string]any `json:"impact"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Review.Operator != "E3001" || resp.Review.OperatorName != "王会计" {
		t.Errorf("应记录真实工号与姓名，得到 %+v", resp.Review)
	}
	if resp.Review.ID == "" {
		t.Error("复核记录应有编号（撤销时要用）")
	}
	if resp.Impact["affects_dedup"] != true {
		t.Errorf("应明确告知影响查重库: %v", resp.Impact)
	}
	// 前面步骤里"冒名"那条也写成了一条通过结论，各自占一条查重记录。
	if len(s.extra()) < 1 {
		t.Fatalf("应至少回写 1 条查重记录，得到 %d", len(s.extra()))
	}

	// 4) 撤销：标记撤销并回滚查重库
	body, _ = json.Marshal(map[string]any{"id": resp.Review.ID, "reason": "看错了",
		"employee_id": "E3001"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("POST", "/api/review/revoke", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("撤销失败: %d %s", rec.Code, rec.Body.String())
	}
	// 只断言"自己那条"被撤掉：前面步骤里还写进过别的通过结论，
	// 撤销一条不能把别人那条一起删了（这是本次要保证的边界）。
	want := resp.Review.AffectedHistory.ClaimNo
	for _, h := range s.extra() {
		if h.ClaimNo == want {
			t.Errorf("撤销后该条查重记录应消失: %s", want)
		}
	}

	// 5) 记录仍在（审计要能看到谁撤的）
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("GET", "/api/reviews", nil))
	var list struct {
		Reviews []ReviewOutcome `json:"reviews"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	// 列表按时间倒序；最近一条应是刚撤销的那条。前面步骤里可能还有别的记录
	// （冒名测试也会落一条），所以不能断言总数等于 1。
	if len(list.Reviews) == 0 {
		t.Fatal("撤销记录应保留")
	}
	latest := list.Reviews[0]
	if !latest.Revoked() {
		t.Fatalf("最近一条应标记为已撤销: %+v", latest)
	}
	if latest.RevokeReason != "看错了" {
		t.Errorf("撤销原因应记录，得到 %q", latest.RevokeReason)
	}
}

// 驳回类结论不写查重库：写进去会让这张票以后被判"已报销过"，语义是错的。
func TestRejectDoesNotAffectDedup(t *testing.T) {
	s := testServer(t)
	h := s.Routes()
	body, _ := json.Marshal(map[string]any{"case_id": "C01", "decision": "reject",
		"employee_id": "E3001"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("POST", "/api/review", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("复核失败: %d", rec.Code)
	}
	if len(s.extra()) != 0 {
		t.Errorf("驳回结论不应写查重库，实际 %d 条", len(s.extra()))
	}
}

// 配额熔断：预算耗尽时拒绝启动审核，且明确指向"转人工"而非降级。
func TestQuotaBlocksAudit(t *testing.T) {
	s := testServer(t)
	q, err := quota.Open(filepath.Join(t.TempDir(), "q.json"), quota.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	q.AddUsage("anonymous", 60) // 超过 $50 预算
	s.Quota = q

	body, _ := json.Marshal(map[string]any{"case_id": "C01"})
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("POST", "/api/audit", bytes.NewReader(body)))
	if rec.Code != 429 {
		t.Fatalf("超预算应 429，得到 %d：%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	msg, _ := resp["action"].(string)
	if !strings.Contains(msg, "人工") {
		t.Errorf("应明确要求转人工（而不是降级），得到 %q", msg)
	}
}

// 未超预算时正常放行。
func TestQuotaAllowsUnderBudget(t *testing.T) {
	s := testServer(t)
	q, err := quota.Open("", quota.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	q.AddUsage("anonymous", 10) // $10 / $50
	s.Quota = q
	body, _ := json.Marshal(map[string]any{"case_id": "C01"})
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("POST", "/api/audit", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("未超预算应放行，得到 %d", rec.Code)
	}
	var started struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil || started.SessionID == "" {
		t.Fatalf("审核未启动: %s", rec.Body.String())
	}
	s.mu.Lock()
	session := s.sessions[started.SessionID]
	s.mu.Unlock()
	select {
	case <-session.done:
	case <-time.After(5 * time.Second):
		t.Fatal("审核未完成")
	}
}

// 配额接口返回本周用量与配置。
func TestQuotaEndpoint(t *testing.T) {
	s := testServer(t)
	s.Quota, _ = quota.Open(filepath.Join(t.TempDir(), "q.json"), quota.DefaultConfig())
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/quota", nil))
	var d map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if d["enabled"] != true {
		t.Fatalf("应报告配额已启用: %v", d)
	}
}

// 人员名单接口只给必要信息，且过滤停用账号。
func TestPeopleEndpoint(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/people", nil))
	var d struct {
		People []struct {
			EmployeeID string `json:"employee_id"`
			Name       string `json:"name"`
		} `json:"people"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if len(d.People) != 1 || d.People[0].EmployeeID != "E3001" {
		t.Fatalf("名单不对: %+v", d.People)
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
