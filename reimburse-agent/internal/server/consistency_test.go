package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 落盘失败时不能留下半成功状态。
//
// 场景：复核通过的结论要同时写"记录"和"查重回写"两张表。
// 如果写盘失败却只回滚了一半，界面上显示复核成功、内存里也有记录，
// 但重启后这条结论没了——财务以为通过了，系统下次还会把同一张票放行。
// 这里把文件换成不可写路径，确认接口报错且内存干净。
func TestReviewPersistFailureLeavesNothing(t *testing.T) {
	s := testServer(t)
	dir := t.TempDir()

	// 先用一个正常文件让 store 建立，再把这个路径变成目录，使后续写入必然失败。
	blocked := filepath.Join(dir, "reviews.json")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	// OpenPath 此时会尝试 saveLocked，写目录必然失败。
	if err := s.OpenReviewStore(filepath.Join(dir, "sub", "reviews.json")); err != nil {
		t.Fatal(err)
	}
	// 把路径指向一个"是目录"的位置，制造写失败。
	s.reviews.mu.Lock()
	s.reviews.path = blocked
	s.reviews.mu.Unlock()

	before := len(s.Reviews())
	body, _ := json.Marshal(map[string]any{"case_id": "C01", "decision": "approve", "employee_id": "E3001"})
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, localReq("POST", "/api/review", bytes.NewReader(body)))

	if rec.Code == 200 {
		t.Fatalf("落盘失败不应返回成功: %s", rec.Body.String())
	}
	if rec.Code != 503 {
		t.Errorf("落盘失败应返回 503（服务端问题），得到 %d", rec.Code)
	}
	// 内存必须干净：既没有新增记录，也没有查重回写。
	if got := len(s.Reviews()); got != before {
		t.Errorf("落盘失败后内存记录不应增加：%d → %d", before, got)
	}
	if n := len(s.extra()); n != 0 {
		t.Errorf("落盘失败后查重库应为空，实际 %d 条", n)
	}
}

// 并发启动：审核可能瞬间跑完，参数必须在启动前就位。
//
// 之前的 Start() 先起 goroutine、再让调用方设 OnFinish/TaskID，
// 审核快的时候回调还没挂，任务状态就不会更新——"转人工后待办消失"就是这么来的。
// 这里用极快的合成审核反复触发，断言每一轮都落到了任务上。
func TestStartAppliesTaskEvenWhenFast(t *testing.T) {
	s := testServer(t)
	h := s.Routes()

	for i := 0; i < 5; i++ {
		body, _ := json.Marshal(map[string]any{"case_id": "C01"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, localReq("POST", "/api/audit", bytes.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("第 %d 次启动失败: %d", i+1, rec.Code)
		}
		var start struct {
			TaskID string `json:"task_id"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &start)

		// 等到这一轮落进任务的批次数里。
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if tk, ok := s.Tasks.Get(start.TaskID); ok && len(tk.Runs) >= i+1 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		tk, _ := s.Tasks.Get(start.TaskID)
		if len(tk.Runs) < i+1 {
			t.Fatalf("第 %d 轮审核没有落到任务上（批次数 %d）：OnFinish 未生效", i+1, len(tk.Runs))
		}
		if tk.LastDecision == "" {
			t.Errorf("第 %d 轮结论为空，说明状态没同步", i+1)
		}
	}
}

// 先后两次通过、撤销旧意见后，最新那条仍应阻止重复报销。
//
// 这是"撤销"最容易写错的地方：一撤就把整张票从查重库删了，
// 于是第二次通过的结论也跟着失效，同一张票又能再报一次。
func TestRevokeOldKeepsLatestDedup(t *testing.T) {
	s := testServer(t)
	h := s.Routes()

	post := func(decision, note string) ReviewOutcome {
		body, _ := json.Marshal(map[string]any{"case_id": "C01", "claim_no": "BX26110001",
			"decision": decision, "note": note, "employee_id": "E3001"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, localReq("POST", "/api/review", bytes.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("复核失败: %d %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Review ReviewOutcome `json:"review"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return resp.Review
	}

	first := post("approve", "第一次通过")
	second := post("approve", "复核后再次通过")

	// 撤销旧的那条。
	body, _ := json.Marshal(map[string]any{"id": first.ID, "reason": "撤掉旧的", "employee_id": "E3001"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("POST", "/api/review/revoke", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("撤销失败: %d %s", rec.Code, rec.Body.String())
	}

	// 最新的那条仍有效，查重库必须还在。
	if n := len(s.extra()); n != 1 {
		t.Fatalf("撤销旧意见后，最新通过仍应保留查重记录，实际 %d 条", n)
	}
	// 且那条记录应来自最新一次复核。
	hist := s.extra()[0]
	// 主键带复核记录编号：撤销旧的那条只删 #RV0001，留下 #RV0002。
	if want := "BX26110001-RV#" + second.ID; hist.ClaimNo != want {
		t.Errorf("保留的应是最近一次通过的那条，期望 %s，实际 %s", want, hist.ClaimNo)
	}
	latest := s.Reviews()[0]
	if latest.ID != second.ID || latest.Revoked() {
		t.Errorf("最新一条不应被撤销: %+v", latest)
	}
	// 再次提交同样内容应判为重复（说明最新结论确实有效）。
	again := post("approve", "复核后再次通过")
	if !again.Duplicate || again.ID != second.ID {
		t.Errorf("最新结论应仍然生效并挡住重复提交，实际 %+v", again)
	}
}
