package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"reimburse/internal/llm"
	"reimburse/internal/schema"
	"reimburse/plugins/reimburse-audit/judge"
	"reimburse/plugins/reimburse-audit/policy"
)

// 编排层：把一次审核拆成可观测的阶段，并保证任何一处失败都落在"转人工"上，不落在"通过"上。
//
// 形态上是固定流水线而不是自由规划的 ReAct 循环，这是有意为之：
// 报销审核是对错分明的业务，让模型自己决定调什么工具、什么时候结束，
// 就出现了"跳过一个必查项也能给出通过"的可能。固定步骤 + 每步留痕，
// 既能保证必查项一个不漏，也满足"过程可观测"的要求。

// Stage 是一个执行阶段。
type Stage string

const (
	StagePlan      Stage = "plan"      // 确定要检查什么
	StageExtract   Stage = "extract"   // 双模型抽取，交叉比对
	StageTools     Stage = "tools"     // 调用 MCP 工具核验外部事实
	StageAggregate Stage = "aggregate" // 执行规则、汇总结论
	StageExplain   Stage = "explain"   // 生成审核意见文字
	StageDone      Stage = "done"
)

// Step 是流水线中的一个步骤定义，用于界面展示"计划"。
type Step struct {
	Stage  Stage  `json:"stage"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// Plan 返回本次审核的步骤清单。它由规则表推导，而不是写死：
// 哪几条规则要调哪个工具，就从规则表的 tools 字段取。
func Plan(rf *policy.File, cat string) []Step {
	tools := map[string][]string{}
	for _, r := range rf.Rules {
		if !appliesTo(r, cat) {
			continue
		}
		for _, t := range r.Tools {
			tools[t] = append(tools[t], r.ID)
		}
	}
	names := []string{"check_company", "check_history", "verify_invoice"}
	var toolParts []string
	for _, t := range names {
		if ids, ok := tools[t]; ok {
			toolParts = append(toolParts, fmt.Sprintf("%s（%s）", toolLabel(t), strings.Join(ids, "/")))
		}
	}
	ruleCount := 0
	for _, r := range rf.Rules {
		if appliesTo(r, cat) {
			ruleCount++
		}
	}
	return []Step{
		{StagePlan, "确定检查范围", fmt.Sprintf("%s类共 %d 条规则适用，需调用 %d 个外部工具", judgeCategoryCN(cat), ruleCount, len(toolParts))},
		{StageExtract, "双模型抽取发票字段", "DeepSeek 与 Claude 各自独立抽取，逐字段比对；不一致或置信度低于 " +
			fmt.Sprintf("%.2f", rf.Params.LowConfidenceThreshold) + " 的字段标为不可信"},
		{StageTools, "核验外部事实", strings.Join(toolParts, "；")},
		{StageAggregate, "执行规则判定", "按规则表逐条判定，命中项全部列出，结论取最严格者"},
		{StageExplain, "生成审核意见", "由规则结果驱动，条款编号与金额在生成后回查核对"},
	}
}

func appliesTo(r policy.Rule, cat string) bool {
	for _, c := range r.AppliesTo {
		if c == cat {
			return true
		}
	}
	return false
}

func toolLabel(t string) string {
	switch t {
	case "verify_invoice":
		return "发票查验"
	case "check_company":
		return "企业核验"
	case "check_history":
		return "历史查重"
	}
	return t
}

// Reviewer 是运行期开关：某个模型被关掉时模拟"不可用"，用于现场演示降级路径。
type Reviewer struct {
	LLM      LLM
	Client   *llm.Client
	Cfg      *llm.Config
	Disabled map[string]bool
	// Replay 不为空时，被关闭或无法调用的模型改用录制回放；回放结果必须在界面上标明来源。
	Replay *llm.Replayer
	// CaseID 用于寻址录制文件。
	CaseID string
	// Recorded 记录本次真正走了回放的供应商。
	Recorded []string
	// ToolFaults 是演示开关：模拟某个 MCP 工具故障，用于现场演示"工具挂了会怎样"。
	// 真实故障由 MCP 服务自己产生，这里与真实故障等价，两者可叠加。
	ToolFaults map[string]Status
}

// Emitter 接收事件。事件是审核过程的唯一真相来源：界面、日志、回放都从它渲染。
type Emitter func(Event)

// Event 是一条过程事件。
type Event struct {
	Seq        int       `json:"seq"`
	At         time.Time `json:"at"`
	Stage      Stage     `json:"stage"`
	Kind       string    `json:"kind"` // stage / llm_call / llm_result / tool_call / tool_result / finding / decision / warn
	Title      string    `json:"title"`
	Detail     string    `json:"detail,omitempty"`
	Data       any       `json:"data,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
}

// Report 是一次审核的完整结果。
type Report struct {
	ClaimNo        string                 `json:"claim_no"`
	ClaimAmountFen int64                  `json:"claim_amount_fen"`
	Plan           []Step                 `json:"plan"`
	Extract        *llm.DualResult        `json:"extract"`
	Tools          map[string]ToolOutcome `json:"tools"`
	Result         *Result                `json:"result"`
	Opinion        string                 `json:"opinion"`
	Events         []Event                `json:"events"`
	CostUSD        float64                `json:"cost_usd"`
	ElapsedMS      int64                  `json:"elapsed_ms"`
	// Warnings 是降级说明，例如只有一边模型可用、工具超时。
	Warnings []string `json:"warnings"`
	// Trace 是逐字段溯源：每个值在原件里出自哪一句，找不到的单独标出。
	Trace []FieldTrace `json:"field_trace,omitempty"`
	// TaskID 是本次审核关联的任务号（有任务库时）。
	TaskID string `json:"task_id,omitempty"`
	// TaskSyncError 非空表示结论已出，但任务状态没同步成功。
	// 界面必须据此提示"待办可能不同步"，不能让财务以为流程已经推进。
	TaskSyncError string `json:"task_sync_error,omitempty"`
}

// Options 控制一次审核的行为。
type Options struct {
	// Injected 是演示用的故障注入：工具名 -> 状态。真实故障由工具层自己产生，二者可叠加。
	Injected map[string]Status
	// Explain 用哪个模型写审核意见；空表示不生成，界面直接用规则依据拼接。
	Explain string
	// OnEvent 为空时只收集事件不回调。
	OnEvent Emitter
	// Recheck 是续审计划：补件后哪些判定项要重查、哪些已核验结果可复用。
	// 为空表示这是首次审核。
	Recheck *RecheckPlan
	// ResumeHint 是一句话说明本轮是第几轮续审，写进事件流开头。
	ResumeHint string
}

// RecheckPlan 是续审的重查计划（与 task.Recheck 结构一致，避免编排层依赖任务包）。
type RecheckPlan struct {
	CheckIDs     []string
	KeepVerified []string
	Reverify     []string
	Reason       string
}

// Run 执行一次完整审核。
func Run(e *Engine, ctx context.Context, cl *schema.Claim, invoiceText, supportText string, ex Explorer, rv *Reviewer, opt Options) *Report {
	start := time.Now()
	// 演示开关与命令行的故障注入合并；显式注入优先。
	if rv != nil && len(rv.ToolFaults) > 0 {
		if opt.Injected == nil {
			opt.Injected = map[string]Status{}
		}
		for k, v := range rv.ToolFaults {
			if _, ok := opt.Injected[k]; !ok {
				opt.Injected[k] = v
			}
		}
	}
	rep := &Report{ClaimNo: cl.ClaimNo, ClaimAmountFen: cl.AmountFen, Tools: map[string]ToolOutcome{}, Plan: Plan(e.RF, cl.Category)}
	// 事件序号在并发抽取时由多个 goroutine 产生，用原子计数保证唯一，最终按产生顺序排序。
	var seq int64
	emit := func(stage Stage, kind, title, detail string, data any, d time.Duration) {
		n := atomic.AddInt64(&seq, 1)
		ev := Event{Seq: int(n), At: time.Now(), Stage: stage, Kind: kind, Title: title, Detail: detail, Data: data,
			DurationMS: d.Milliseconds()}
		rep.Events = append(rep.Events, ev)
		if opt.OnEvent != nil {
			opt.OnEvent(ev)
		}
	}

	emit(StagePlan, "stage", "确定检查范围", rep.Plan[0].Detail, rep.Plan, 0)
	// 续审时把"复用什么、重查什么"摆出来，让读者一眼看出这不是从头再跑一遍。
	if opt.Recheck != nil {
		detail := opt.ResumeHint
		if opt.Recheck.Reason != "" {
			detail += "。" + opt.Recheck.Reason
		}
		emit(StagePlan, "stage", "续审计划", detail, opt.Recheck, 0)
	}

	// 1. 双模型抽取
	src := llm.Source{Category: cl.Category, InvoiceText: invoiceText, SupportText: supportText}
	dual := dualExtract(e, ctx, src, rv, func(provider string, phase string, resp *llm.Response, issue *llm.SchemaIssue, d time.Duration) {
		switch phase {
		case "start":
			emit(StageExtract, "llm_call", providerName(provider)+" 开始抽取", "", nil, 0)
		case "ok":
			title := providerName(provider) + " 抽取完成"
			detail := fmt.Sprintf("输入 %d / 输出 %d token，耗时 %dms，重试 %d 次",
				resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.LatencyMS, resp.Attempts-1)
			if resp.Model == "synthetic" {
				// 合成数据必须一眼能看出来，不能让演示被误读成"真实模型跑出来的"。
				title = providerName(provider) + " 抽取完成（合成演示数据）"
				detail += "；本次为合成数据，非模型真实输出"
			}
			emit(StageExtract, "llm_result", title,
				detail, fieldTable(resp), d)
			// 只有真实模型调用才计费；回放与合成不产生费用，不能计入成本。
			if rv.Cfg != nil && resp.Model != "synthetic" {
				if ep, ok := rv.Cfg.Endpoint(provider); ok {
					rep.CostUSD += ep.CostUSD(resp.Usage.InputTokens, resp.Usage.OutputTokens)
				}
			}
		case "replay":
			emit(StageExtract, "warn", providerName(provider)+" 无实时结果，改用录制回放", "回放数据来自此前一次真实调用，并非本次实时输出", nil, d)
		case "fail":
			emit(StageExtract, "warn", providerName(provider)+" 抽取失败", issue.Detail, issue, d)
		}
	})
	rep.Extract = dual
	for _, f := range llm.ExtractFields {
		v := dual.Fields[f]
		state := "可信"
		switch {
		case v.Value == "":
			state = "未读到，相关判定项不作判定"
		case contains(dual.Untrusted, f):
			state = "两模型不一致，标红转人工"
		case contains(dual.LowConf, f):
			state = "置信度不足，标红转人工"
		}
		emit(StageExtract, "llm_result", "字段 "+judgeFieldCN(judge.FieldName(f)),
			fmt.Sprintf("合并值「%s」置信度 %.2f，%s", v.Value, v.Conf, state), v, 0)
	}
	for _, n := range []string{"A", "B"} {
		ex := dual.A
		if n == "B" {
			ex = dual.B
		}
		if ex != nil && ex.Model == "synthetic" {
			rep.Warnings = append(rep.Warnings, "本次抽取使用合成演示数据（"+n+" 侧），不代表任何模型的实际能力")
		}
	}
	if dual.Degraded != "" {
		rep.Warnings = append(rep.Warnings, dual.Degraded+"，关键字段缺少交叉校验，按未核验转人工")
		// 单模型可用时，把"没有交叉校验"显式传给规则层：
		// 两个模型都成功时，不一致的字段已经通过 Untrusted 表达了。
		dual.LowConf = append(dual.LowConf, LowConfidenceNoCrossCheck)
	}

	// 2. 工具核验。只对可信且非空的键调用，避免拿猜的值去查库。
	tools := map[string]ToolOutcome{}
	callTool := func(name string, fn func() ToolOutcome) ToolOutcome {
		if st, ok := opt.Injected[name]; ok && st != Failed {
			emit(StageTools, "tool_call", toolLabel(name)+" 被注入"+judgeToolCN(st), "演示用故障注入", nil, 0)
		}
		t0 := time.Now()
		emit(StageTools, "tool_call", "调用 "+name, "", nil, 0)
		tr := fn()
		tr = inject(tr, opt.Injected[name])
		emit(StageTools, "tool_result", toolLabel(name)+" 返回 "+judgeToolCN(tr.Status.Status), toolSummary(tr), tr, time.Since(t0))
		if !tr.Status.Status.Usable() {
			rep.Warnings = append(rep.Warnings, toolLabel(name)+" "+judgeToolCN(tr.Status.Status)+"，相关判定项不作判定")
		}
		return tr
	}
	invNo := dual.Fields[llm.FInvoiceNo]
	sellerTax := dual.Fields[llm.FSellerTaxID]
	if invNo.Value != "" && !contains(dual.Untrusted, llm.FInvoiceNo) && invNo.Conf >= e.RF.Params.LowConfidenceThreshold {
		tools["verify_invoice"] = callTool("verify_invoice", func() ToolOutcome { return ex.VerifyInvoice(ctx, invNo.Value) })
		// 查重按规则需要的最少条件：发票号 + 申请人 + 销售方 + 开票日期。
		tools["check_history"] = callTool("check_history", func() ToolOutcome {
			return ex.CheckHistory(ctx, HistoryQuery{
				InvoiceNo: invNo.Value, ApplicantID: cl.Applicant.EmployeeID,
				SellerTaxID: sellerTax.Value, IssueDate: dual.Fields[llm.FIssueDate].Value,
			})
		})
	} else {
		tools["verify_invoice"] = ToolOutcome{Status: judge.Verdict{Status: Failed, Reason: "发票号不可信，无从查验"}}
		tools["check_history"] = ToolOutcome{Status: judge.Verdict{Status: Failed, Reason: "发票号不可信，无从查重"}}
	}
	if sellerTax.Value != "" && !contains(dual.Untrusted, llm.FSellerTaxID) && sellerTax.Conf >= e.RF.Params.LowConfidenceThreshold {
		tools["check_company"] = callTool("check_company", func() ToolOutcome { return ex.CheckCompany(ctx, sellerTax.Value) })
	} else {
		tools["check_company"] = ToolOutcome{Status: judge.Verdict{Status: Failed, Reason: "销售方税号不可信，无从核验"}}
	}
	rep.Tools = tools

	// 3. 规则判定
	// 把宿主类型翻译成插件要求的类型（见 adapter.go）。
	fields, untrusted, low := toJudgeFields(dual)
	in := &Input{Claim: ToJudgeClaim(cl), Fields: fields, Untrusted: untrusted, LowConfidence: low,
		SupportText: supportText, Tools: tools}
	res := e.Judge(in)
	rep.Result = res
	for _, f := range res.Findings {
		emit(StageAggregate, "finding", f.CheckID+" "+f.RuleName+" → "+outcomeCN(f.Outcome), f.Reason, f, 0)
	}
	var skipped []string
	for id := range res.Skipped {
		skipped = append(skipped, id)
	}
	sort.Strings(skipped)
	for _, id := range skipped {
		emit(StageAggregate, "warn", id+" 未判定", res.Skipped[id], nil, 0)
	}
	emit(StageAggregate, "decision", "结论："+outcomeCN(res.Decision),
		fmt.Sprintf("命中 %d 项，核准金额 %s", len(res.Hits), fen(res.ApprovedFen)), res, 0)

	// 4. 审核意见：先从规则结果生成确定性文本，能写清就不必调模型。
	rep.Opinion = composeOpinion(cl, res, e.RF)
	if opt.Explain != "" && rv != nil && rv.LLM != nil {
		text, issue := explain(e, ctx, cl, res, rep.Opinion, rv, opt.Explain, func(s string) {
			emit(StageExplain, "llm_call", "生成审核意见", s, nil, 0)
		})
		if issue == nil {
			rep.Opinion = text
			emit(StageExplain, "llm_result", "审核意见已生成", "条款编号与金额已回查核对", text, 0)
		} else {
			rep.Warnings = append(rep.Warnings, "审核意见润色失败，改用规则依据拼接的文本："+issue.Detail)
			emit(StageExplain, "warn", "审核意见润色失败", "已降级为规则依据拼接", issue, 0)
		}
	} else {
		emit(StageExplain, "llm_result", "审核意见由规则依据拼接", "未调用模型，避免模型改动金额或条款", nil, 0)
	}

	// 5. 字段溯源：把每个值回到原件里指出处。
	//    放在最后做，因为它需要判定结果里的字段四态；且不影响判定。
	rep.Trace = TraceFields(rep, invoiceText, supportText)
	if n := countUntraced(rep.Trace); n > 0 {
		emit(StageAggregate, "warn", fmt.Sprintf("%d 个字段在原件中未找到原文", n),
			"可能是模型补全或格式改写，已在结果里逐项标出，建议人工核对", nil, 0)
	}

	rep.ElapsedMS = time.Since(start).Milliseconds()
	emit(StageDone, "decision", "完成", fmt.Sprintf("总耗时 %dms，估算成本 $%.5f", rep.ElapsedMS, rep.CostUSD), nil, 0)
	// 并发产生的序号可能乱序落盘，这里按序号排序，保证事件流可顺序回放。
	sort.SliceStable(rep.Events, func(i, j int) bool { return rep.Events[i].Seq < rep.Events[j].Seq })
	return rep
}

// dualExtract 在 Reviewer 允许的模型范围内做双抽取；被关闭的模型按失败处理。
func dualExtract(e *Engine, ctx context.Context, src llm.Source, rv *Reviewer, cb func(string, string, *llm.Response, *llm.SchemaIssue, time.Duration)) *llm.DualResult {
	out := &llm.DualResult{Fields: map[string]llm.FieldValue{}}
	names := []string{llm.ProviderDeepSeek, llm.ProviderClaude}
	type res struct {
		provider string
		ex       *llm.Extraction
		issue    *llm.SchemaIssue
	}
	// 无缓冲通道按完成顺序汇总；事件按序号排序，界面看到的顺序与真实调用顺序一致。
	ch := make(chan res, 2)
	for _, n := range names {
		go func(n string) {
			if rv.Disabled[n] {
				cb(n, "start", nil, nil, 0)
				cb(n, "fail", nil, &llm.SchemaIssue{Kind: llm.ErrDisabled, Detail: "已被演示开关关闭"}, 0)
				ch <- res{n, nil, &llm.SchemaIssue{Kind: llm.ErrDisabled, Detail: "已被演示开关关闭"}}
				return
			}
			t0 := time.Now()
			cb(n, "start", nil, nil, 0)
			ex, issue := rv.LLM.ExtractWith(ctx, n, src, nil)
			// 实时调用不可用时，才尝试录制回放；有密钥时永远优先真实调用。
			if issue != nil && rv.Replay != nil && rv.CaseID != "" {
				if rex, rissue := rv.Replay.ReplayExtract(ctx, n, rv.CaseID, nil); rissue == nil {
					ex, issue = rex, nil
					rv.Recorded = append(rv.Recorded, n)
					cb(n, "replay", nil, nil, 0)
				}
			}
			if issue != nil {
				cb(n, "fail", nil, issue, time.Since(t0))
			} else {
				cb(n, "ok", &llm.Response{Provider: ex.Provider, Model: ex.Model, Usage: ex.Usage,
					LatencyMS: ex.LatencyMS, Attempts: ex.Attempts}, nil, time.Since(t0))
			}
			ch <- res{n, ex, issue}
		}(n)
	}
	// 按通道到达顺序收齐再按名字归位。
	// 早先用切片顺序直接当 A/B，出错时会张冠李戴：把 Claude 的结果记成 DeepSeek，
	// 界面上就会出现"关闭 Claude 却说只有 Claude 成功"这种自相矛盾的提示。
	got := []res{<-ch, <-ch}
	for _, r := range got {
		switch r.provider {
		case llm.ProviderDeepSeek:
			out.A, out.IssueA = r.ex, r.issue
		case llm.ProviderClaude:
			out.B, out.IssueB = r.ex, r.issue
		}
	}
	out.Compare()
	return out
}

func inject(tr ToolOutcome, st Status) ToolOutcome {
	if st == 0 || st.Usable() {
		return tr
	}
	return ToolOutcome{Status: judge.Verdict{Status: st, Reason: "演示注入的" + judgeToolCN(st)}}
}

func providerName(p string) string {
	if p == llm.ProviderDeepSeek {
		return "DeepSeek"
	}
	return "Claude"
}

func outcomeCN(o string) string {
	switch o {
	case policy.Approve:
		return "通过"
	case policy.ApproveAdj:
		return "调整后通过"
	case policy.Reject:
		return "驳回"
	case policy.ManualReview:
		return "转人工复核"
	case policy.NeedMoreInfo:
		return "退回补材料"
	}
	return o
}

func fieldTable(r *llm.Response) map[string]string {
	return map[string]string{"raw": truncate(r.Text, 400)}
}

func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

func toolSummary(tr ToolOutcome) string {
	switch tr.Status.Status {
	case Found:
		var parts []string
		if n := len(tr.Invoices); n > 0 {
			parts = append(parts, fmt.Sprintf("%d 条发票登记", n))
		}
		if n := len(tr.Companies); n > 0 {
			parts = append(parts, fmt.Sprintf("%d 条企业信息", n))
		}
		if n := len(tr.History); n > 0 {
			parts = append(parts, fmt.Sprintf("%d 条历史记录", n))
		}
		if len(parts) == 0 {
			return "查无记录"
		}
		return strings.Join(parts, "，")
	}
	return tr.Status.Reason
}

// composeOpinion 从规则结果生成确定性审核意见。金额与条款编号都直接取自规则表和本次判定，
// 不经过模型改写，因此不会出现"文字里的金额和判决不一致"。
func composeOpinion(cl *schema.Claim, res *Result, rf *policy.File) string {
	var b strings.Builder
	fmt.Fprintf(&b, "单据 %s（%s，%s，申请 %s）：结论为「%s」。", cl.ClaimNo, judgeCategoryCN(cl.Category), cl.Applicant.Name,
		fen(cl.AmountFen), outcomeCN(res.Decision))
	if len(res.Findings) == 0 {
		b.WriteString("全部适用规则均未命中，金额与凭证一致，可直接通过。")
	} else {
		b.WriteString("\n\n依据：")
		for i, f := range res.Findings {
			fmt.Fprintf(&b, "\n%d. %s《%s》%s：%s", i+1, f.CheckID, f.RuleName, f.Clause, f.Reason)
		}
	}
	switch res.Decision {
	case policy.Approve:
		fmt.Fprintf(&b, "\n\n核准金额 %s。", fen(res.ApprovedFen))
	case policy.ApproveAdj:
		fmt.Fprintf(&b, "\n\n核准金额 %s（按标准上限调整，超出部分自理）。", fen(res.ApprovedFen))
	case policy.NeedMoreInfo:
		b.WriteString("\n\n请补充上述材料后重新提交。")
	case policy.ManualReview:
		b.WriteString("\n\n已转人工复核，本系统不自动放行。")
	case policy.Reject:
		b.WriteString("\n\n不符合报销规定，予以驳回。")
	}
	if len(res.Skipped) > 0 {
		var ids []string
		for id := range res.Skipped {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		b.WriteString("\n\n本次未作判定的项（需人工确认）：")
		for i, id := range ids {
			if i > 0 {
				b.WriteString("；")
			}
			fmt.Fprintf(&b, "%s %s", id, res.Skipped[id])
		}
	}
	if len(res.Verified) > 0 {
		b.WriteString("\n\n已核验：")
		b.WriteString(strings.Join(res.Verified, "；"))
		b.WriteString("。")
	}
	if len(res.Unverified) > 0 {
		fmt.Fprintf(&b, "\n\n未核验的外部事实：%s。", strings.Join(res.Unverified, "、"))
	}
	return b.String()
}

// explainSystem 让模型只做"把规则结论写成通顺的审核意见"，并明确禁止改动数字和条款。
const explainSystem = `你是财务共享中心的审核员。下面给出系统已经判定好的结论、命中条款和金额。
你的任务只是把这段内容改写成一段通顺、专业、可以直接发给员工的审核意见。

硬性要求：
1. 不得改变结论、条款编号、金额、日期。所有数字只能照抄，不得换算、估算或补充。
2. 不得新增未列出的问题，也不得删掉任何一条命中项。
3. 只输出 json，不要输出其他文字。格式示例：
{"opinion":"字串"}`

var opinionSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"properties": map[string]any{
		"opinion": map[string]any{"type": "string", "description": "审核意见正文"},
	},
	"required": []string{"opinion"},
}

// explain 生成润色后的审核意见，并在生成后做一次机器核对：
// 意见里出现的每个金额都必须能在规则结果中找到，出现的每个判定项编号都必须真实命中。
// 核对不过就丢弃模型文本，用确定性草稿——宁可文字生硬，也不让文字和判决对不上。
func explain(e *Engine, ctx context.Context, cl *schema.Claim, res *Result, draft string, rv *Reviewer, provider string, onStart func(string)) (string, *llm.SchemaIssue) {
	onStart("提交规则结论，要求不改动金额与条款")
	resp := rv.Client.Call(ctx, provider, &llm.Request{
		System:   explainSystem,
		Messages: []llm.Message{{Role: "user", Content: "已判定结果如下（草稿，仅可改写措辞）：\n\n" + draft}},
		Schema:   opinionSchema, SchemaName: "review_opinion",
	})
	if resp.Err != nil {
		return "", &llm.SchemaIssue{Kind: resp.Kind, Detail: resp.Err.Error()}
	}
	b, err := llm.ExtractJSON(resp.Resp.Text)
	if err != nil {
		return "", &llm.SchemaIssue{Kind: llm.ErrBadJSON, Detail: err.Error()}
	}
	var out struct {
		Opinion string `json:"opinion"`
	}
	if err := json.Unmarshal(b, &out); err != nil || strings.TrimSpace(out.Opinion) == "" {
		return "", &llm.SchemaIssue{Kind: llm.ErrBadJSON, Detail: "opinion 字段缺失或为空"}
	}
	if why := verifyOpinion(res, out.Opinion); why != "" {
		return "", &llm.SchemaIssue{Kind: llm.ErrBadJSON, Detail: "模型改动了数字或条款：" + why}
	}
	return out.Opinion, nil
}

var reMoney = regexp.MustCompile(`¥\s?\d[\d,]*\.\d{2}`)
var reCheckID = regexp.MustCompile(`R\d{2}-[a-z]`)

// verifyOpinion 核对意见文本中的金额和条款编号。
func verifyOpinion(res *Result, text string) string {
	allowed := map[string]bool{}
	add := func(v int64) { allowed[fen(v)] = true; allowed[strings.ReplaceAll(fen(v), "¥", "")] = true }
	add(res.ApprovedFen)
	add(res.ClaimAmountFen)
	for _, f := range res.Findings {
		for _, m := range reMoney.FindAllString(f.Reason, -1) {
			allowed[m] = true
			allowed[strings.ReplaceAll(m, "¥", "")] = true
			allowed[strings.ReplaceAll(m, " ", "")] = true
			allowed[strings.ReplaceAll(strings.ReplaceAll(m, "¥", ""), " ", "")] = true
		}
	}
	// 已核验事实里出现的金额（例如登记的价税合计）也允许引用。
	for _, ev := range res.Evidence {
		for _, m := range reMoney.FindAllString(ev, -1) {
			allowed[m] = true
			allowed[strings.ReplaceAll(m, "¥", "")] = true
		}
	}
	for _, m := range reMoney.FindAllString(text, -1) {
		if allowed[m] || allowed[strings.ReplaceAll(m, "¥", "")] {
			continue
		}
		return "出现了未在判定结果中的金额 " + m
	}
	hit := map[string]bool{}
	for _, id := range res.Hits {
		hit[id] = true
	}
	for _, id := range reCheckID.FindAllString(text, -1) {
		if !hit[id] {
			return "引用了未命中的判定项 " + id
		}
	}
	return ""
}

// contains 判断字符串切片是否包含某值。
func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// containsStr 同 contains，保留旧名以便调用点不改。
func containsStr(xs []string, x string) bool { return contains(xs, x) }

// 下面三个是插件导出能力的薄封装：编排层展示时需要中文名，但不该自己再维护一份。
func judgeCategoryCN(c string) string       { return judge.CategoryCN(c) }
func judgeFieldCN(f judge.FieldName) string { return judge.FieldCN(f) }
func judgeToolCN(s judge.Status) string     { return judge.ToolCN(s) }

// fen 把分格式化为 ¥xx.yy，供审核意见与事件使用。
func fen(v int64) string {
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("¥%s%d.%02d", sign, v/100, v%100)
}
