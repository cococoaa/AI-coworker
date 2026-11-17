package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"reimburse/internal/audit"
	"reimburse/internal/task"
	"reimburse/plugins/reimburse-audit/judge"
)

// 缺材料 → 补交 → 自动续审 的闭环测试。
//
// 这是"能持续承接工作"的最小证明：系统要知道单子卡在哪、缺什么、
// 补交后该重查哪几项，而不是每次都从零跑一遍。

// waitTask 轮询直到任务进入某个状态。
func waitTask(t *testing.T, s *Server, id string, want task.State, within time.Duration) *task.Task {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if tk, ok := s.Tasks.Get(id); ok && tk.State == want {
			return tk
		}
		time.Sleep(50 * time.Millisecond)
	}
	tk, _ := s.Tasks.Get(id)
	if tk != nil {
		t.Fatalf("等待任务进入 %s 超时，当前 %s", want, tk.State)
	}
	t.Fatalf("任务 %s 不存在", id)
	return nil
}

// 缺佐证凭证时，任务应退回补材料，并给出补件清单与续审计划。
//
// 这里不跑完整审核链路（那依赖模型是否可用），而是直接喂一份判定结果——
// 本测试要验证的是"结论如何落到任务状态上"，不是模型能力。
func TestTaskNeedMoreProducesMissingList(t *testing.T) {
	s := testServer(t)
	s.Tasks, _ = task.Open(t.TempDir() + "/tasks.json")
	c, ok := s.Find("C44") // C44 缺佐证凭证
	if !ok {
		t.Skip("缺少 C44")
	}
	tk, err := s.Tasks.Create(c.ID, c.Claim.ClaimNo, c.Claim.Applicant.EmployeeID)
	if err != nil {
		t.Fatal(err)
	}

	// 构造一份"缺佐证凭证"的判定结果（与真实审核对 R14-a 的判定一致）。
	in := &audit.Input{
		Claim:  audit.ToJudgeClaim(&c.Claim),
		Tools:  map[string]audit.ToolOutcome{},
		Fields: judge.Fields{},
	}
	res := &audit.Result{
		Decision: "need_more_info", Hits: []string{"R14-a"},
		Skipped:     map[string]string{"R08-a": "无行程单日期", "R09-a": "行程单缺少席别"},
		FieldStatus: judge.Statuses{},
	}
	missing := buildMissing(in, res)
	recheck := buildRecheck(in, res)
	if _, err := s.Tasks.ApplyDecision(tk.ID, task.Run{Decision: "need_more_info"}, "need_more_info", missing, recheck, "E3001"); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Tasks.Get(tk.ID)
	if got.State != task.NeedMore {
		t.Fatalf("应为待补材料，得到 %s", got.State)
	}
	if got.State.WaitingOn() != "报销人补交材料" {
		t.Errorf("应指明卡在报销人那里，得到 %q", got.State.WaitingOn())
	}
	out := got.Outstanding()
	if len(out) == 0 {
		t.Fatal("待补材料状态必须有补件清单")
	}
	found := false
	for _, m := range out {
		if m.Key == "support" {
			found = true
			if m.Kind != KindAttachment {
				t.Errorf("缺佐证凭证应标为 missing_attachment，得到 %s", m.Kind)
			}
			if m.Reason == "" || m.Clause == "" {
				t.Error("补件项必须写明原因与依据条款")
			}
		}
	}
	if !found {
		t.Errorf("补件清单里应含佐证凭证，实际 %+v", out)
	}
	// 续审计划：因缺件跳过的判定项要重查。
	if got.NextRecheck == nil || len(got.NextRecheck.CheckIDs) == 0 {
		t.Errorf("应给出续审要重查的判定项: %+v", got.NextRecheck)
	}
}

// 补交必须带正文；只给文件名不给内容等于没补，续审结论不会变。
func TestResubmitRequiresContent(t *testing.T) {
	s := testServer(t)
	s.Tasks, _ = task.Open(t.TempDir() + "/tasks.json")
	tk, _ := s.Tasks.Create("C44", "BX1", "E1001")
	s.Tasks.ApplyDecision(tk.ID, task.Run{Decision: "need_more_info"}, "need_more_info",
		[]task.MissingItem{{Key: "support", Label: "行程单", Kind: KindAttachment}}, nil, "E3001")

	h := s.Routes()
	// 只给文件名，不给正文
	body, _ := json.Marshal(map[string]any{"kind": "support", "name": "x.pdf", "employee_id": "E3001"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/tasks/"+tk.ID+"/resubmit", strings.NewReader(string(body))))
	if rec.Code != 400 {
		t.Fatalf("缺正文应拒绝，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "续审时判定不会变化") {
		t.Errorf("应说明为什么拒绝: %s", rec.Body.String())
	}
}

// 补交后状态回到待审核，并且可以续审。
func TestResubmitBringsBackToPending(t *testing.T) {
	s := testServer(t)
	s.Tasks, _ = task.Open(t.TempDir() + "/tasks.json")
	tk, _ := s.Tasks.Create("C44", "BX1", "E1001")
	s.Tasks.ApplyDecision(tk.ID, task.Run{Decision: "need_more_info"}, "need_more_info",
		[]task.MissingItem{{Key: "support", Label: "行程单", Kind: KindAttachment}}, nil, "E3001")

	h := s.Routes()
	body, _ := json.Marshal(map[string]any{
		"kind": "support", "name": "行程单.pdf", "employee_id": "E1001", "content": "铁路电子客票\n乘车人：张三\n"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/tasks/"+tk.ID+"/resubmit", strings.NewReader(string(body))))
	if rec.Code != 200 {
		t.Fatalf("补交失败: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Task *task.Task     `json:"task"`
		Next map[string]any `json:"next"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Task.State != task.Pending {
		t.Errorf("补齐后应回到待审核，得到 %s", resp.Task.State)
	}
	// 下一步应明确是"续审"。
	if act, _ := resp.Next["action"].(string); act != "reaudit" {
		t.Errorf("下一步应提示续审，得到 %q", act)
	}
}

// 不是待补材料状态的任务拒绝补交。
func TestResubmitRejectedWhenNotNeedMore(t *testing.T) {
	s := testServer(t)
	s.Tasks, _ = task.Open(t.TempDir() + "/tasks.json")
	tk, _ := s.Tasks.Create("C01", "BX1", "E1001")
	s.Tasks.ApplyDecision(tk.ID, task.Run{Decision: "approve"}, "approve", nil, nil, "E3001")

	h := s.Routes()
	body, _ := json.Marshal(map[string]any{"kind": "support", "name": "x.pdf", "employee_id": "E1001", "content": "x"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/tasks/"+tk.ID+"/resubmit", strings.NewReader(string(body))))
	if rec.Code != 400 {
		t.Fatalf("已通过的任务不应接受补交，得到 %d", rec.Code)
	}
}

// 待办列表要能按状态筛，并给出"卡在谁那里、卡了几天"。
func TestTaskListShowsWaiting(t *testing.T) {
	s := testServer(t)
	s.Tasks, _ = task.Open(t.TempDir() + "/tasks.json")
	a, _ := s.Tasks.Create("C44", "BX1", "E1001")
	s.Tasks.ApplyDecision(a.ID, task.Run{Decision: "need_more_info"}, "need_more_info",
		[]task.MissingItem{{Key: "support", Label: "行程单", Kind: KindAttachment}}, nil, "E3001")
	b, _ := s.Tasks.Create("C51", "BX2", "E1002")
	s.Tasks.ApplyDecision(b.ID, task.Run{Decision: "manual_review"}, "manual_review", nil, nil, "E3001")

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/tasks?waiting=1", nil))
	var d struct {
		Tasks []map[string]any `json:"tasks"`
		Stats map[string]int   `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Tasks) != 2 {
		t.Fatalf("应列出 2 张在途单据，得到 %d", len(d.Tasks))
	}
	for _, tk := range d.Tasks {
		if tk["waiting_on"] == "" || tk["waiting_on"] == "未知" {
			t.Errorf("每张单都要说明卡在谁那里: %v", tk)
		}
	}
	// 按状态筛
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/tasks?state=need_manual", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if len(d.Tasks) != 1 {
		t.Errorf("按 need_manual 筛应只有 1 张，得到 %d", len(d.Tasks))
	}
}

// 续审计划要区分"重查"与"复用"，这是增量续审的核心。
// 这里用一张工具全部成功的单据，直接看 buildRecheck 的产出。
func TestRecheckPlanSeparatesReverifyAndKeep(t *testing.T) {
	s := testServer(t)
	c, ok := s.Find("C01")
	if !ok {
		t.Skip("缺少 C01")
	}
	// 手工构造一份"三家工具都成功"的判定结果。
	res := &audit.Result{
		Skipped: map[string]string{
			"R05-a": "非业务招待",         // 不适用，不该进续审清单
			"R02-b": "字段 购买方税号 无法识别", // 缺件，应进续审清单
		},
		Hits: []string{"R14-a"},
	}
	in := &audit.Input{
		Claim: audit.ToJudgeClaim(&c.Claim),
		Tools: map[string]audit.ToolOutcome{
			"verify_invoice": {Status: judge.Verdict{Status: judge.Found}, Invoices: []judge.InvoiceRecord{{InvoiceNo: "x"}}},
			"check_company":  {Status: judge.Verdict{Status: judge.Found}, Companies: []judge.CompanyRecord{{TaxID: "y"}}},
			"check_history":  {Status: judge.Verdict{Status: judge.Found}, History: []judge.HistoryRecord{{ClaimNo: "z"}}},
		},
	}
	plan := buildRecheck(in, res)
	if plan == nil {
		t.Fatal("应生成续审计划")
	}
	if !contains(plan.Reverify, "verify_invoice") {
		t.Errorf("发票查验应重查: %v", plan.Reverify)
	}
	if !contains(plan.Reverify, "check_history") {
		t.Errorf("查重应重查: %v", plan.Reverify)
	}
	if !contains(plan.KeepVerified, "check_company") {
		t.Errorf("企业状态可复用: %v", plan.KeepVerified)
	}
	// 缺件的进清单，不适用的不进。
	if !contains(plan.CheckIDs, "R02-b") {
		t.Errorf("缺件的判定项应进续审清单: %v", plan.CheckIDs)
	}
	if contains(plan.CheckIDs, "R05-a") {
		t.Errorf("不适用的判定项不应进续审清单: %v", plan.CheckIDs)
	}
}

// 补件清单要能区分"原件没有"与"看不清"——它们要求的动作不同。
func TestMissingKindsDistinguishAbsentFromUnreadable(t *testing.T) {
	s := testServer(t)
	c, ok := s.Find("C24")
	if !ok {
		t.Skip("缺少 C24")
	}
	in := &audit.Input{Claim: audit.ToJudgeClaim(&c.Claim)}
	res := &audit.Result{
		FieldStatus: judge.Statuses{
			judge.FBuyerTaxID:  judge.Absent,
			judge.FSellerTaxID: judge.Unreadable,
		},
		Hits:    []string{"R14-b"},
		Skipped: map[string]string{},
	}
	items := buildMissing(in, res)
	byKey := map[string]task.MissingItem{}
	for _, m := range items {
		byKey[m.Key] = m
	}
	if m, ok := byKey["buyer_tax_id"]; !ok {
		t.Error("购买方税号缺失应进补件清单")
	} else if m.Kind != KindAbsent {
		t.Errorf("原件没有应标 absent，得到 %s", m.Kind)
	}
	if m, ok := byKey["seller_tax_id"]; !ok {
		t.Error("销售方税号看不清应进补件清单")
	} else if m.Kind != KindUnreadable {
		t.Errorf("看不清应标 unreadable，得到 %s", m.Kind)
	}
}

// contains 判断字符串切片是否含某值。
func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}
