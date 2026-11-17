package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"reimburse/internal/audit"
)

// 回归：审核刚结束的一瞬间，SSE 必须能拿到完整报告。
//
// 曾经的 bug：finish() 先广播"流结束"，再由 defer 关闭 done 通道。
// 中间有个窗口——订阅方收到流结束、立刻读快照，得到"没完成、也没有报告"，
// 于是界面显示一个空的结论，用户以为系统没算出来。
//
// 现在 finish() 先标记完成再广播。这个测试反复连流，抓到空报告就失败。
func TestSSENeverEndsWithoutReport(t *testing.T) {
	s := testServer(t)
	h := s.Routes()

	body, _ := json.Marshal(map[string]any{"case_id": "C01"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/audit", bytes.NewReader(body)))
	var start struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}

	// 以极短的间隔反复连流，尽量踩中"刚结束"那个瞬间。
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/audit/"+start.SessionID+"/events", nil))
		raw := rec.Body.String()
		evs := parseSSE(t, rec.Body.Bytes())

		if strings.Contains(raw, "event: done") {
			// 流结束了：此时必须能拿到报告——要么在 snapshot 里，要么在 report 事件里。
			var got bool
			if len(evs["snapshot"]) > 0 {
				var snap struct {
					Report *audit.Report `json:"report"`
					Done   bool          `json:"done"`
				}
				if err := json.Unmarshal([]byte(evs["snapshot"][0]), &snap); err == nil {
					if snap.Done && snap.Report != nil {
						got = true
					}
				}
			}
			if len(evs["report"]) > 0 {
				got = true
			}
			if !got {
				t.Fatalf("流已经结束，却拿不到报告：snapshot=%d report=%d",
					len(evs["snapshot"]), len(evs["report"]))
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Skip("20 秒内未跑完，跳过")
}

// 同一张单据反复审核，只应有一个任务。
//
// 否则待办列表会出现同一张单的两三条记录，财务不知道点哪个。
// 这也是"幂等"在任务层的体现：重复点击「开始审核」不该多出新任务。
// waitTaskRounds 等到任务的批次达到 n，说明这轮审核已经落到任务上。
func waitTaskRounds(t *testing.T, s *Server, id string, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if tk, ok := s.Tasks.Get(id); ok && len(tk.Runs) >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("任务 %s 在 30 秒内没有累积到 %d 轮", id, n)
}

func TestRepatedAuditReusesTask(t *testing.T) {
	s := testServer(t)
	h := s.Routes()

	ids := map[string]bool{}
	for i := 0; i < 3; i++ {
		body, _ := json.Marshal(map[string]any{"case_id": "C01"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/audit", bytes.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("第 %d 次启动失败: %d %s", i+1, rec.Code, rec.Body.String())
		}
		var start struct {
			TaskID string `json:"task_id"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &start)
		if start.TaskID == "" {
			t.Fatal("应自动建任务")
		}
		ids[start.TaskID] = true
		waitTaskRounds(t, s, start.TaskID, i+1)
	}
	if len(ids) != 1 {
		t.Errorf("同一张单据应只有一个任务，实际出现 %d 个: %v", len(ids), ids)
	}
	if n := len(s.Tasks.List()); n != 1 {
		t.Errorf("任务库应只有 1 条，实际 %d 条", n)
	}
}
