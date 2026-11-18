package server

import (
	"fmt"

	"reimburse/internal/schema"
	"reimburse/internal/task"
	"reimburse/plugins/reimburse-audit/policy"
)

func isApproval(decision string) bool {
	return decision == policy.Approve || decision == policy.ApproveAdj
}

func (s *Server) activeApproval(caseID string) (ReviewOutcome, bool) {
	if s.reviews != nil {
		for _, r := range s.Reviews() {
			if r.CaseID == caseID && !r.Revoked() && r.Decision != policy.ManualReview {
				return r, isApproval(r.Decision)
			}
		}
	}
	return ReviewOutcome{}, false
}

// syncTaskApproval applies the effective finance decision to the corresponding task.
// Repeating this after a restart or client retry leaves prior audit batches intact.
func (s *Server) syncTaskApproval(c *schema.Case) error {
	if s.Tasks == nil {
		return nil
	}
	r, approved := s.activeApproval(c.ID)
	t, found := s.Tasks.FindByCase(c.ID)
	if !approved || !found || t.State == task.Withdrawn ||
		(t.State == task.Approved && t.ApprovalReviewID == r.ID) {
		return nil
	}
	_, err := s.Tasks.ApplyReviewApproval(t.ID, r.ID, r.Decision, r.Operator, r.Note, c.Claim.AmountFen)
	return err
}

// RepairReviewApprovals reconciles earlier approvals that were stored without updating the task.
func (s *Server) RepairReviewApprovals() error {
	if s.Tasks == nil {
		return nil
	}
	for _, t := range s.Tasks.List() {
		if c, ok := s.Find(t.CaseID); ok {
			if err := s.syncTaskApproval(c); err != nil {
				return fmt.Errorf("%s: %w", t.ID, err)
			}
		}
	}
	return nil
}
