package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"reimburse/internal/llm"
	"reimburse/internal/task"
	"reimburse/plugins/reimburse-audit/policy"
)

func TestTodayAuditChatCountsDistinctCasesAndFollowsUp(t *testing.T) {
	s := testServer(t)
	s.DemoSynthetic = true
	now := time.Now()
	record := func(caseID string, at time.Time, outcomes ...string) {
		t.Helper()
		row, err := s.Tasks.Create(caseID, caseID, "demo")
		if err != nil {
			t.Fatal(err)
		}
		for _, decision := range outcomes {
			run := task.Run{At: at, Decision: decision}
			if _, err := s.Tasks.ApplyDecision(row.ID, run, decision, nil, nil, "test"); err != nil {
				t.Fatal(err)
			}
		}
	}
	record("daily-a", now, policy.ManualReview, policy.Approve)
	record("daily-b", now, policy.ManualReview)
	record("previous-day", now.AddDate(0, 0, -1), policy.ManualReview)
	if cases, rounds, manual := auditTotals(s.Tasks.List(), now); cases != 2 || rounds != 3 || manual != 1 {
		t.Fatalf("got cases=%d rounds=%d manual=%d", cases, rounds, manual)
	}
	h := s.Routes()
	ask := func(messages ...llm.Message) string {
		t.Helper()
		body, err := json.Marshal(map[string]any{"messages": messages})
		if err != nil {
			t.Fatal(err)
		}
		req := localReq("POST", "/api/chat", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var reply map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK || reply["mode"] != "business" {
			t.Fatalf("expected an answer from the audit store, got %d %+v", rec.Code, reply)
		}
		return reply["reply"]
	}
	first := llm.Message{Role: "user", Content: "你今天审了多少单子了，累么"}
	answer := ask(first)
	if !strings.Contains(answer, "2 张单，共 3 轮") || !strings.Contains(answer, "1 张转人工") {
		t.Fatalf("unexpected tally: %q", answer)
	}
	next := ask(first, llm.Message{Role: "assistant", Content: answer},
		llm.Message{Role: "user", Content: "其中有多少要人工复审？"})
	if !strings.Contains(next, "有 1 张") {
		t.Fatalf("unexpected follow-up: %q", next)
	}
}
