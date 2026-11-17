package server

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"reimburse/internal/audit"
	"reimburse/internal/schema"
	"reimburse/internal/task"
	"reimburse/plugins/reimburse-audit/judge"
)

// 从判定结果推出"缺什么"和"续审要重查什么"。
//
// 这是闭环的关键一步：结论是"退回补材料"还不够，必须回答三个问题——
//  1. 缺的是哪几项？每项为什么缺（原件没有 / 看不清 / 两模型不一致）？
//  2. 补交之后要重查哪几项判定？
//  3. 哪些已核验结果可以复用（不重查），哪些必须重查（有时效性）？
//
// 第 1 问的答案来自 judge.Result.FieldStatus（字段四态）与 Skipped（因缺件跳过的判定项）；
// 第 2 问由 Skipped 反查即可；第 3 问按工具是否"有时效性"判断。

// 缺失项类型，与 judge 的字段四态对应，另加"缺附件"一类。
const (
	KindAbsent     = "absent"             // 原件确实没有
	KindUnreadable = "unreadable"         // 看不清
	KindAmbiguous  = "ambiguous"          // 两模型不一致
	KindAttachment = "missing_attachment" // 缺整份材料（如出差审批单）
)

// buildMissing 从判定结果生成补件清单。
func buildMissing(in *audit.Input, res *audit.Result) []task.MissingItem {
	out := make([]task.MissingItem, 0, 4)
	seen := map[string]bool{}

	// 1) 字段层面：原件没有 / 看不清 / 不一致。
	//    只有"关键字段"才进补件清单——非关键字段缺失不影响判定，列出来只会干扰。
	key := map[judge.FieldName]bool{}
	for _, f := range judge.KeyFields() {
		key[f] = true
	}
	for _, f := range judge.FieldNames() {
		st := res.FieldStatus.Of(f)
		if st == judge.Present || st == judge.Unknown {
			continue
		}
		if !key[f] && st != judge.Ambiguous {
			continue
		}
		k := string(f)
		if seen[k] {
			continue
		}
		seen[k] = true
		item := task.MissingItem{Key: k, Label: judge.FieldCN(f)}
		switch st {
		case judge.Absent:
			item.Kind = KindAbsent
			item.Reason = "发票上未填写该栏（原件缺失，不是识别问题）"
		case judge.Unreadable:
			item.Kind = KindUnreadable
			item.Reason = "发票上该栏无法辨认，需重新提供清晰的原件"
		case judge.Ambiguous:
			item.Kind = KindAmbiguous
			item.Reason = "两个模型对该字段的抽取结果不一致，需人工核对"
		}
		// 关联到具体条款：哪条判定项因它而跳过。
		item.Clause = clauseForSkipped(in, res, f)
		out = append(out, item)
	}

	// 2) 材料层面：缺佐证凭证整份。
	if res.FieldStatus != nil {
		// R14-a 命中说明缺佐证凭证。
		if hasHit(res.Hits, "R14-a") {
			label := supportLabel(in.Claim.Category)
			k := "support"
			if !seen[k] {
				seen[k] = true
				out = append(out, task.MissingItem{
					Key: k, Label: label, Kind: KindAttachment,
					Reason: "缺少对应类别的" + label + "，无法核对费用日期与金额",
					Clause: "第2.7条",
				})
			}
		}
	}

	// 3) 报销单信息不全（缺申请单号、事由、人数等）。
	if hasHit(res.Hits, "R14-b") {
		if !seen["claim_info"] {
			seen["claim_info"] = true
			out = append(out, task.MissingItem{
				Key: "claim_info", Label: "报销单信息", Kind: KindAbsent,
				Reason: "报销单必填项不全（申请单号 / 事由 / 人数 / 招待对象单位之一）",
				Clause: "第2.7条",
				Hint:   claimInfoHint(in.Claim),
			})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// buildRecheck 算出续审的重查计划。
//
// 规则：
//   - 因缺件而跳过的判定项 → 必须重查（它们是这次没判的）
//   - 发票查验 → 重查（有时效性，且发票可能被重开）
//   - 企业核验 → 可复用（企业状态短期内不变），但若已超过 7 天则重查
//   - 历史查重 → 重查（期间可能有人报过同一张票）
func buildRecheck(in *audit.Input, res *audit.Result) *task.Recheck {
	r := &task.Recheck{}

	// 1) 因缺件跳过的判定项：全部要重查。
	//    排除"不适用"的跳过（如"非住宿类"），那不是缺件。
	var needRecheck []string
	for id, why := range res.Skipped {
		if isNotApplicable(why) {
			continue
		}
		needRecheck = append(needRecheck, id)
	}
	sort.Strings(needRecheck)
	r.CheckIDs = needRecheck

	// 2) 工具层面：哪些需要重新核验。
	var reverify []string
	var keep []string
	for _, t := range []string{"verify_invoice", "check_company", "check_history"} {
		tr, ok := in.Tools[t]
		if !ok {
			continue
		}
		switch t {
		case "verify_invoice":
			// 发票查验必须重查：发票可能被重开、作废，且平台状态会变。
			reverify = append(reverify, t)
			if tr.Status.Status == judge.Found {
				r.Reason = "已核验的发票信息会在续审时重新查验（平台状态可能变化）"
			}
		case "check_history":
			// 查重必须重查：期间可能有人报过同一张票。
			reverify = append(reverify, t)
		case "check_company":
			// 企业状态可短期复用。
			if tr.Status.Status == judge.Found {
				keep = append(keep, t)
			} else {
				reverify = append(reverify, t)
			}
		}
	}
	r.Reverify = reverify
	r.KeepVerified = keep
	if r.Reason == "" {
		r.Reason = "补件后只重查与缺件相关的判定项，已核验且未过期的事实直接复用"
	}
	return r
}

// isNotApplicable 判断一条跳过原因是不是"规则不适用"（而非缺件）。
// 这类不应该进续审计划——补件也不会让它们变成适用。
func isNotApplicable(why string) bool {
	for _, kw := range []string{"不适用", "非住宿类", "非餐饮类", "非交通类", "非业务招待", "餐饮不核对本人",
		"已查，一致", "同上（已核验）", "同上（已查验）", "本单已达审批线", "已填事前审批号"} {
		if strings.Contains(why, kw) {
			return true
		}
	}
	return false
}

// clauseForSkipped 找出因某字段而跳过的判定项，返回其条款编号。
func clauseForSkipped(in *audit.Input, res *audit.Result, f judge.FieldName) string {
	// 从 Skipped 里找提到该字段名的项。
	name := judge.FieldCN(f)
	var ids []string
	for id, why := range res.Skipped {
		if strings.Contains(why, name) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	sort.Strings(ids)
	return ids[0]
}

// claimInfoHint 按单据类别给出"该补哪些键"，并说明格式。
// 不列全所有键：只列这张单真正缺的，避免让人以为要填一堆无关字段。
func claimInfoHint(cl *judge.Claim) string {
	type kv struct{ k, label string }
	var need []kv
	if cl.RequestNo == "" {
		need = append(need, kv{"request_no", "申请单号"})
	}
	if cl.Category == "meal" {
		if cl.Purpose == "" {
			need = append(need, kv{"purpose", "事由"})
		}
		if cl.InternalCount+cl.ExternalCount == 0 {
			need = append(need, kv{"internal_count", "内部人数"})
		}
		if cl.ExternalCount > 0 && cl.ExternalOrg == "" {
			need = append(need, kv{"external_org", "招待对象单位"})
		}
	}
	if len(need) == 0 {
		return "按 键=值 逐行填写"
	}
	var parts []string
	for _, n := range need {
		parts = append(parts, fmt.Sprintf("%s=…（%s）", n.k, n.label))
	}
	return "逐行填写：" + strings.Join(parts, "　")
}

func supportLabel(cat string) string {
	switch cat {
	case "transport":
		return "行程单"
	case "lodging":
		return "酒店水单"
	case "meal":
		return "消费清单"
	}
	return "佐证凭证"
}

func hasHit(hits []string, id string) bool {
	for _, h := range hits {
		if h == id {
			return true
		}
	}
	return false
}

// convertRecheck 把任务层计划转成编排层计划。
func convertRecheck(r *task.Recheck) *audit.RecheckPlan {
	if r == nil {
		return nil
	}
	return &audit.RecheckPlan{
		CheckIDs: r.CheckIDs, KeepVerified: r.KeepVerified,
		Reverify: r.Reverify, Reason: r.Reason,
	}
}

// applyReportToTask 把一次审核结论落到任务状态上。
//
// 几件事必须一起做，否则闭环会断：
//   - 结论映射成状态（need_more_info → 待补材料，并带上补件清单）
//   - 算出下一轮的续审计划（补交后要重查什么）
//   - 记录本批次的成本与耗时（便于算"这张单花了多少钱"）
func (s *Server) applyReportToTask(taskID string, rep *audit.Report, c *schema.Case) {
	if s.Tasks == nil || rep == nil || rep.Result == nil {
		return
	}
	in := &audit.Input{
		Claim:       toJudgeClaimForMissing(c),
		Fields:      toJudgeFieldsFromReport(rep),
		Untrusted:   map[judge.FieldName]bool{},
		SupportText: c.SupportText,
		Tools:       rep.Tools,
	}
	missing := buildMissing(in, rep.Result)
	recheck := buildRecheck(in, rep.Result)

	run := task.Run{
		Decision: rep.Result.Decision, Hits: rep.Result.Hits, Skipped: rep.Result.Skipped,
		ApprovedFen: rep.Result.ApprovedFen, CostUSD: rep.CostUSD, ElapsedMS: rep.ElapsedMS,
	}
	// 触发方式按是否已有批次判断：第一次是 initial，之后是续审。
	if n := s.taskRunCount(taskID); n > 0 {
		run.Trigger = "reaudit"
	} else {
		run.Trigger = "initial"
	}
	_, err := s.Tasks.ApplyDecision(taskID, run, rep.Result.Decision, missing, recheck, "system")
	if err != nil {
		// 任务状态没更新，待办就不会出现在列表里——"转人工后待办消失"就是这么来的。
		// 不能只在报告里加一行警告（没人看得到），要写成醒目的失败事件，
		// 并标记报告有未完成的状态同步，让界面能提示"结论已出，但任务未同步"。
		msg := "任务状态更新失败（结论已出，但待办可能不同步）：" + err.Error()
		rep.Warnings = append(rep.Warnings, msg)
		rep.TaskSyncError = err.Error()
		rep.Events = append(rep.Events, audit.Event{
			Seq:    len(rep.Events) + 1,
			Stage:  audit.StageDone,
			Kind:   "fail",
			Title:  "任务状态未同步",
			Detail: msg,
			Data:   map[string]any{"task_id": taskID, "error": err.Error()},
		})
		return
	}
	rep.TaskID = taskID
}

// releaseTask 把卡在"审核中"的任务放回待审核，让它可以重试。
//
// 一经抢占就必须有释放路径：审核中途失败（工具连不上、进程被杀）时，
// 任务会停在 Auditing，界面上既不能续审也不能重新审核，只能手工改文件。
func (s *Server) releaseTask(id string) {
	if s.Tasks == nil {
		return
	}
	_, _ = s.Tasks.Update(id, func(t *task.Task) error {
		if t.State == task.Auditing {
			t.State = task.Pending
			t.Events = append(t.Events, task.Event{At: time.Now(), Kind: "audit_failed",
				Detail: "审核异常中断，已放回待审核"})
		}
		return nil
	})
}

// taskRunCount 返回任务已有的审核批次数。
func (s *Server) taskRunCount(id string) int {
	if t, ok := s.Tasks.Get(id); ok {
		return len(t.Runs)
	}
	return 0
}

// toJudgeClaimForMissing 从一个用例的报销单构造补件判定用的输入。
func toJudgeClaimForMissing(c *schema.Case) *judge.Claim {
	return audit.ToJudgeClaim(&c.Claim)
}

// toJudgeFieldsFromReport 从报告里取回合并后的字段（含 presence）。
func toJudgeFieldsFromReport(rep *audit.Report) judge.Fields {
	out := judge.Fields{}
	if rep.Extract == nil {
		return out
	}
	for _, name := range judge.FieldNames() {
		fv, ok := rep.Extract.Fields[string(name)]
		if !ok {
			continue
		}
		out[name] = judge.Field{Value: fv.Value, Conf: fv.Conf, Presence: fv.Presence}
	}
	return out
}

var _ = fmt.Sprintf
