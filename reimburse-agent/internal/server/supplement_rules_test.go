package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"reimburse/internal/task"
	"reimburse/plugins/reimburse-audit/judge"
	"reimburse/plugins/reimburse-audit/policy"
)

func TestEveryReturnRuleHasSupplement(t *testing.T) {
	rf, err := policy.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range rf.AllCheckIDs() {
		_, check, _ := rf.FindCheck(id)
		if check.Outcome != policy.NeedMoreInfo {
			continue
		}
		items := ruleSupplementItems(&judge.Claim{Category: "transport"}, []string{id})
		if len(items) != 1 || items[0].Reason == "" || items[0].Hint == "" {
			t.Errorf("%s lacks a usable submission item: %+v", id, items)
		}
	}
}

func TestLegacyLateTaskCanSubmitAndReaudit(t *testing.T) {
	s := testServer(t)
	s.DemoSynthetic = true
	c := mustCase(t, "C11")
	original, err := s.Tasks.Create(c.ID, c.Claim.ClaimNo, c.Claim.Applicant.EmployeeID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Tasks.ApplyDecision(original.ID, task.Run{Decision: policy.NeedMoreInfo, Hits: []string{"R07-a"}}, policy.NeedMoreInfo, nil, nil, "system")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.RepairSupplementTasks(); err != nil {
			t.Fatal(err)
		}
	}
	fixed, _ := s.Tasks.Get(original.ID)
	if len(fixed.Runs) != 1 || len(fixed.Missing) != 1 || fixed.Missing[0].Key != "late_explanation" {
		t.Fatalf("legacy task not recovered: %+v", fixed)
	}
	h := s.Routes()
	call := func(path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, localReq(http.MethodPost, "/api/tasks/"+fixed.ID+path, strings.NewReader(body)))
		return rec
	}
	if rec := call("/reaudit", "{}"); rec.Code != 400 {
		t.Fatalf("unresolved task started: %d %s", rec.Code, rec.Body)
	}
	if rec := call("/resubmit", "{\"kind\":\"late_explanation\",\"content\":\"返程后收到票据，故延迟提交。\"}"); rec.Code != 200 {
		t.Fatalf("submission failed: %d %s", rec.Code, rec.Body)
	}
	rec := call("/reaudit", "{}")
	if rec.Code != 200 {
		t.Fatalf("reaudit failed: %d %s", rec.Code, rec.Body)
	}
	var started struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil || started.SessionID == "" {
		t.Fatal(rec.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		updated, _ := s.Tasks.Get(fixed.ID)
		if len(updated.Runs) > 1 {
			if updated.State != task.NeedManual || len(updated.Missing) != 0 || len(updated.Attachments) != 1 {
				t.Fatalf("late explanation did not reach finance: %+v", updated)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("reaudit did not finish")
}
