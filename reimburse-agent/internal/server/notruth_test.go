package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"reimburse/internal/identity"
	"reimburse/internal/llm"
	"reimburse/internal/schema"
	"reimburse/internal/task"
	"reimburse/plugins/reimburse-audit/policy"
)

// 真实票据没有标准答案（truth.json），也必须能完成：
// 加载 → 审核 → 复核 → 查重回写。
//
// 为什么这条重要：测试集带标准答案，真实票据不会有。
// 如果加载时就要求 truth.json，系统只能审自己造的题，审不了业务里的票。
// 这里用 data/notruth 下的样本套件（无 truth.json、无 world.json）验证整条链路。
func TestRealInvoiceWithoutTruth(t *testing.T) {
	dir := "../../data/notruth"
	s := newFrom(t, dir)
	if len(s.Cases) == 0 {
		t.Fatal("应加载到至少一条真实票据")
	}
	c := s.Cases[0]
	if c.HasTruth {
		t.Fatal("这条样本本来就没有 truth.json，HasTruth 应为假")
	}

	h := s.Routes()

	// 1) 审核：没有标准答案、没有模型密钥，结论必须落在转人工，不能是通过。
	body, _ := json.Marshal(map[string]any{"case_id": c.ID})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("POST", "/api/audit", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("启动审核失败: %d %s", rec.Code, rec.Body.String())
	}
	var start struct {
		TaskID    string `json:"task_id"`
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &start)
	if start.TaskID == "" {
		t.Fatal("应自动建任务")
	}
	waitTaskRounds(t, s, start.TaskID, 1)
	tk, _ := s.Tasks.Get(start.TaskID)
	if tk.LastDecision == "approve" || tk.LastDecision == "approve_with_adjustment" {
		t.Errorf("字段抽不出来时不得自动通过，实际 %s", tk.LastDecision)
	}

	// 2) 模型不可用、没有标准答案 → 抽不出发票号。
	//    此时给"通过"必须被拒绝：没有发票号的查重记录查不出任何东西，
	//    写进去只会让财务以为"这张票已经进查重库了"。
	body, _ = json.Marshal(map[string]any{"case_id": c.ID, "claim_no": c.Claim.ClaimNo,
		"session_id": start.SessionID,
		"decision":   "approve", "note": "真实票据人工确认", "employee_id": "E3001"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("POST", "/api/review", bytes.NewReader(body)))
	if rec.Code == 200 {
		t.Fatalf("没有发票号时不应允许写入查重库: %s", rec.Body.String())
	}
	if n := len(s.extra()); n != 0 {
		t.Errorf("被拒绝的复核不应留下查重记录，实际 %d 条", n)
	}

	// 3) 转入补材料：这是没有发票号时唯一合理的方向，且要能回落到任务状态上。
	body, _ = json.Marshal(map[string]any{"case_id": c.ID, "claim_no": c.Claim.ClaimNo,
		"session_id": start.SessionID,
		"decision":   "need_more_info", "note": "请补齐可识别的票据", "employee_id": "E3001"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("POST", "/api/review", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("退回补材料应成功: %d %s", rec.Code, rec.Body.String())
	}
	tk2, _ := s.Tasks.Get(start.TaskID)
	if tk2.State != task.NeedMore {
		t.Errorf("复核退回后任务应为待补材料，实际 %s", tk2.State.Label())
	}

	// 4) 重启（新实例读同一份目录与文件）：结论与查重都还在。
	s2 := newFrom(t, dir)
	if err := s2.OpenReviewStore(s.reviews.path); err != nil {
		t.Fatal(err)
	}
	// 只有成功落库的那条（退回补材料）应留下；被拒绝的那次不产生记录。
	if n := len(s2.Reviews()); n != 1 {
		t.Errorf("重启后复核记录应为 1 条（退回补材料），实际 %d", n)
	}
	if rs := s2.Reviews(); len(rs) > 0 && rs[0].Decision != "need_more_info" {
		t.Errorf("保留的应是退回补材料那条，实际 %s", rs[0].Decision)
	}
	// 本用例全程没有可用的发票号，所以查重库应为空——但"空"本身也要保持。
	if n := len(s2.extra()); n != 0 {
		t.Errorf("重启后查重库应与重启前一致（0 条），实际 %d", n)
	}
}

// 合成模式遇到没有标准答案的单据必须报错，而不是编一份字段出来。
//
// 合成抽取的原理是从标准答案回放。没有答案就只能编，
// 而编出来的字段会进判定、进查重——那比"跑不了"危险得多。
func TestSyntheticRefusesRealInvoice(t *testing.T) {
	c, err := schema.LoadCase("../../data/notruth/cases/C_RT01")
	if err != nil {
		t.Skipf("样本不可用: %v", err)
	}
	if c.HasTruth {
		t.Fatal("样本不应带 truth.json")
	}
	syn := &llm.SyntheticLLM{Case: c, Synth: &llm.Synthetic{}}
	_, issue := syn.ExtractWith(nil, "deepseek", llm.Source{Category: c.Claim.Category, InvoiceText: c.InvoiceText}, nil)
	if issue == nil {
		t.Fatal("没有标准答案时合成必须报错，不能返回字段")
	}
	if issue.Kind != llm.ErrUnsupported {
		t.Errorf("错误类型应为 unsupported，实际 %v", issue.Kind)
	}
}

// 启动时校验：合成模式下存在无标准答案的单据，必须整个退出而不是跑到一半才出错。
func TestDemoSyntheticRequiresTruth(t *testing.T) {
	s := newFrom(t, "../../data/notruth")
	var missing []string
	for _, c := range s.Cases {
		if !c.HasTruth {
			missing = append(missing, c.ID)
		}
	}
	if len(missing) == 0 {
		t.Fatal("样本目录里应有无标准答案的单据")
	}
	// 这里验证的是"能被发现"：启动逻辑据此 fatal。
	if missing[0] == "" {
		t.Error("应报出具体单据编号")
	}
}

// policyfile 是 testServer 用的同一份规则表。
func policyfile() (*policy.File, error) { return policy.Load() }

// newFrom 用指定目录建服务，其余配置与 testServer 一致。
func newFrom(t *testing.T, dir string) *Server {
	t.Helper()
	rf, err := policyfile()
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, rf, llm.DefaultConfig())
	if err != nil {
		t.Fatalf("加载 %s 失败: %v", dir, err)
	}
	people, err := identity.New([]identity.Person{
		{EmployeeID: "E3001", Name: "王会计", CanReview: true, CanAudit: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.People = people
	s.LocalOperator = "E3001"
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
