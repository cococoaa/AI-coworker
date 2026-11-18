package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"reimburse/internal/audit"
	"reimburse/internal/task"
	"reimburse/plugins/reimburse-audit/policy"
)

func TestApprovalSyncAndSelfDedup(t *testing.T) {
	s := testServer(t)
	c, ok := s.Find("C46")
	if !ok {
		t.Fatal("C46 fixture is missing")
	}
	row, err := s.Tasks.Create(c.ID, c.Claim.ClaimNo, c.Claim.Applicant.EmployeeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tasks.ApplyDecision(row.ID, task.Run{Decision: policy.NeedMoreInfo}, policy.NeedMoreInfo,
		[]task.MissingItem{{Key: "support", Label: "消费清单"}}, nil, "system"); err != nil {
		t.Fatal(err)
	}

	h := s.Routes()
	post := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := localReq("POST", path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := post("/api/review", `{"case_id":"C46","decision":"approve","employee_id":"E3001"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("approval failed: %d %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Review    ReviewOutcome `json:"review"`
		TaskState task.State    `json:"task_state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response.TaskState != task.Approved {
		t.Fatalf("approval did not update the task: %s", rec.Body.String())
	}
	if got, _ := s.Tasks.Get(row.ID); got.State != task.Approved || got.LastDecision != policy.Approve ||
		got.ApprovalReviewID != response.Review.ID || len(got.Runs) != 1 {
		t.Fatalf("task approval and historical rounds do not agree: %+v", got)
	}
	rec = post("/api/audit", `{"case_id":"C46"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "已人工复核通过") {
		t.Fatalf("same case should be protected from re-audit: %d %s", rec.Code, rec.Body.String())
	}

	other := *c
	other.ID = "new-submission"
	other.Claim.ClaimNo = "another-claim"
	for _, sample := range []struct {
		other bool
		want  bool
	}{{false, false}, {true, true}} {
		submission := c
		if sample.other {
			submission = &other
		}
		tools, closeTools, err := s.connectTools(context.Background(), submission)
		if err != nil {
			t.Fatal(err)
		}
		history := tools.CheckHistory(context.Background(), audit.HistoryQuery{InvoiceNo: c.Truth.Invoice.InvoiceNo})
		closeTools()
		found := false
		for _, historyRow := range history.History {
			if historyRow.ClaimNo == response.Review.AffectedHistory.ClaimNo {
				found = true
			}
		}
		if found != sample.want {
			t.Fatalf("same-invoice history for distinct submission=%t: got %t; want %t", sample.other, found, sample.want)
		}
	}
	rec = post("/api/review/revoke", `{"id":"`+response.Review.ID+`","reason":"重审"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoking approval failed: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := s.Tasks.Get(row.ID); got.State != task.Pending || got.ApprovalReviewID != "" || len(got.Runs) != 1 {
		t.Fatalf("revoking the current approval did not reopen the task: %+v", got)
	}
}

func TestRepairReviewApprovalsPreservesOldRounds(t *testing.T) {
	s := testServer(t)
	c, _ := s.Find("C46")
	row, err := s.Tasks.Create(c.ID, c.Claim.ClaimNo, c.Claim.Applicant.EmployeeID)
	if err != nil {
		t.Fatal(err)
	}
	oldRun := task.Run{Decision: policy.Reject, Hits: []string{"R04-a"}}
	if _, err := s.Tasks.ApplyDecision(row.ID, oldRun, policy.Reject, nil, nil, "system"); err != nil {
		t.Fatal(err)
	}
	review, _, err := s.reviews.AddWithHistory(ReviewOutcome{CaseID: c.ID, Decision: policy.Approve, Operator: "E3001"},
		&stubHistory{ClaimNo: c.Claim.ClaimNo + "-RV", InvoiceNo: c.Truth.Invoice.InvoiceNo})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.RepairReviewApprovals(); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.Tasks.Get(row.ID)
	if got.State != task.Approved || got.ApprovalReviewID != review.ID ||
		len(got.Runs) != 1 || got.Runs[0].Decision != policy.Reject {
		t.Fatalf("repair must recover the approval without rewriting audit history: %+v", got)
	}
	reviewEvents := 0
	for _, event := range got.Events {
		if event.Kind == "reviewed" {
			reviewEvents++
		}
	}
	if reviewEvents != 1 {
		t.Fatalf("repair was not idempotent: %d events", reviewEvents)
	}
}
