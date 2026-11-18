package server

import (
	"fmt"
	"strings"

	"reimburse/internal/task"
	"reimburse/plugins/reimburse-audit/judge"
)

// ruleMissingItems is shared by fresh results and tasks created by older builds.
func ruleMissingItems(cl *judge.Claim, hits []string) []task.MissingItem {
	var items []task.MissingItem
	for _, id := range hits {
		var item task.MissingItem
		switch id {
		case "R05-a":
			item = task.MissingItem{Key: "entertain_approval_no", Label: "招待事前审批号", Kind: KindAbsent, Clause: "第4.2条",
				Reason: "业务招待金额达到事前审批线，申请单未填写事前审批号", Hint: "填写该笔招待对应的事前审批编号"}
		case "R07-a":
			item = task.MissingItem{Key: "late_explanation", Label: "超期报销说明", Kind: "explanation", Clause: "第2.6条",
				Reason: "报销提交日期超过规定期限", Hint: "填写延迟提交的实际原因及相关日期，供财务复核"}
		case "R09-a":
			item = task.MissingItem{Key: "special_approval_no", Label: "超标席别特批编号", Kind: KindAbsent, Clause: "第3.2条",
				Reason: "席别超出申请人职级允许范围，申请单未填写特批编号", Hint: "填写该次出行的特批编号"}
		case "R14-a":
			label := supportLabel(cl.Category)
			item = task.MissingItem{Key: "support", Label: label, Kind: KindAttachment, Clause: "第2.7条",
				Reason: "缺少对应类别的" + label + "，无法核对费用日期与金额", Hint: "粘贴佐证凭证正文"}
		case "R14-b":
			item = task.MissingItem{Key: "claim_info", Label: "报销单信息", Kind: KindAbsent, Clause: "第2.7条",
				Reason: "报销单必填项不全", Hint: claimInfoHint(cl)}
		default:
			continue
		}
		items = append(items, item)
	}
	return items
}

func ruleSupplementItems(cl *judge.Claim, hits []string) []task.MissingItem {
	items := ruleMissingItems(cl, hits)
	filtered := items[:0]
	for _, item := range items {
		if item.Key != "late_explanation" || strings.TrimSpace(cl.LateExplanation) == "" {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

// RepairSupplementTasks fills in entries that older versions failed to generate.
// It keeps the task ID, attachments, and previous audit rounds.
func (s *Server) RepairSupplementTasks() error {
	if s.Tasks == nil {
		return nil
	}
	for _, t := range s.Tasks.List() {
		if t.State != task.NeedMore {
			continue
		}
		var items []task.MissingItem
		if c, ok := s.Find(t.CaseID); ok && len(t.Runs) > 0 {
			effective, _ := applySupplements(c, t)
			items = ruleSupplementItems(toJudgeClaimForMissing(effective), t.Runs[len(t.Runs)-1].Hits)
		}
		if len(items) == 0 && len(t.Outstanding()) == 0 {
			reason := "请补交财务要求的说明或材料正文"
			if n := len(t.Events); n > 0 && t.Events[n-1].Kind == "reviewed" {
				reason = t.Events[n-1].Detail
			}
			items = append(items, task.MissingItem{Key: "finance_request", Label: "财务要求的补充材料", Kind: "explanation", Reason: reason})
		}
		existing := make(map[string]bool, len(t.Missing))
		for _, item := range t.Missing {
			existing[item.Key] = true
		}
		var missing []task.MissingItem
		for _, item := range items {
			if !existing[item.Key] {
				missing = append(missing, item)
				existing[item.Key] = true
			}
		}
		if len(missing) == 0 {
			continue
		}
		if _, err := s.Tasks.Update(t.ID, func(current *task.Task) error {
			current.Missing = append(current.Missing, missing...)
			return nil
		}); err != nil {
			return fmt.Errorf("%s: %w", t.ID, err)
		}
	}
	return nil
}
