// Package rehearsal 是现场前的自动彩排。
//
// 三次彩排分别验证三种"一定会被问到"的故障：
//  1. 工具故障：MCP 查验超时，系统必须转人工而不是放行；
//  2. 模型超时：一家模型超时，降级为单模型并转人工；
//  3. 单模型不可用：演示开关关掉一家，结论必须转人工。
//
// 与测试的区别：彩排输出一份可存档的报告（JSON + 人读文本），并检查
// "结论是否落在预期范围""事件流是否完整""是否留下未核验记录"。
package rehearsal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reimburse/internal/audit"
	"reimburse/internal/llm"
	"reimburse/internal/mcptools"
	"reimburse/internal/schema"
	"reimburse/plugins/reimburse-audit/policy"
)

// Case 是一次彩排的结果。
type Case struct {
	Name       string   `json:"name"`
	Purpose    string   `json:"purpose"`
	CaseID     string   `json:"case_id"`
	Got        string   `json:"decision"`
	Want       []string `json:"accept"` // 允许的结论集合
	Hits       []string `json:"hits"`
	Events     int      `json:"events"`
	Unverified []string `json:"unverified,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
	Passed     bool     `json:"passed"`
	FailWhy    string   `json:"fail_reason,omitempty"`
	ElapsedMS  int64    `json:"elapsed_ms"`
}

// Report 是一次完整彩排的输出。
type Report struct {
	At     time.Time `json:"at"`
	AtText string    `json:"at_text"`
	Cases  []Case    `json:"cases"`
	Passed int       `json:"passed"`
	Total  int       `json:"total"`
	AllOK  bool      `json:"all_ok"`
}

// Runner 持有彩排需要的公共依赖。
type Runner struct {
	Root string // 数据根目录（含 cases/）
	RF   *policy.File
	Cfg  llm.Config
	// Synth 为真时用合成抽取（无密钥环境）；否则走真实模型。
	Synth bool
}

// Run 执行三次彩排。
func (r *Runner) Run(ctx context.Context) *Report {
	rep := &Report{At: time.Now(), AtText: time.Now().Format("2006-01-02 15:04:05")}
	rep.Cases = append(rep.Cases,
		r.runOne(ctx, "工具故障：发票查验超时", "查验超时属状态未知，必须转人工，不得当作查无此票驳回",
			"C01", []string{policy.ManualReview}, nil,
			map[string]audit.Status{"verify_invoice": audit.ToolTimeout}),
		r.runOne(ctx, "模型超时：Claude 不可用", "只剩一个模型时关键字段无交叉校验，必须转人工",
			"C01", []string{policy.ManualReview}, []string{"claude"}, nil),
		r.runOne(ctx, "单模型不可用：关闭 DeepSeek", "同上，且要在事件流里看到被关闭的原因",
			"C03", []string{policy.ManualReview}, []string{"deepseek"}, nil),
		// 稳态对照：一切正常时必须按标准答案通过，证明故障场景的转人工不是"永远转人工"。
		r.runOne(ctx, "对照：一切正常", "链路完好时应按标准答案自动通过",
			"C01", []string{policy.Approve}, nil, nil),
	)
	for _, c := range rep.Cases {
		if c.Passed {
			rep.Passed++
		}
	}
	rep.Total = len(rep.Cases)
	rep.AllOK = rep.Passed == rep.Total
	return rep
}

func (r *Runner) runOne(ctx context.Context, name, purpose, caseID string, accept []string,
	disabled []string, faults map[string]audit.Status) (result Case) {

	c := Case{Name: name, Purpose: purpose, CaseID: caseID, Want: accept}
	t0 := time.Now()
	// 必须用命名返回值：早先用 defer 改的是局部变量 c，函数返回副本，
	// 耗时永远写成 0，记录里全是 0ms。命名返回值才能把 defer 的修改带出去。
	defer func() { result.ElapsedMS = time.Since(t0).Milliseconds() }()

	sc, err := schema.LoadCase(filepath.Join(r.Root, "cases", caseID))
	if err != nil {
		c.FailWhy = "读取用例失败：" + err.Error()
		return c
	}

	cfg := r.Cfg
	var llmImpl audit.LLM = llm.NewClient(&cfg)
	rv := &audit.Reviewer{LLM: llmImpl, Cfg: &cfg, Disabled: map[string]bool{}}
	for _, d := range disabled {
		rv.Disabled[d] = true
	}
	if r.Synth {
		rv.LLM = &llm.SyntheticLLM{Case: sc, Synth: &llm.Synthetic{}, Fallback: llmImpl}
	}

	// 工具：进程内 MCP 服务；用例自带的故障 + 本次彩排的注入。
	fm := map[string]string{}
	for _, f := range sc.World.Faults {
		fm[f.Tool+":"+f.Key] = f.Mode
	}
	for tool, st := range faults {
		// 按工具名注入时对所有键生效，构造一个通配键即可（服务端先查 tool:key 再查 tool）。
		mode := "timeout"
		if st == audit.ToolFailed {
			mode = "error"
		}
		fm[tool] = mode
	}
	srv := mcptools.NewServer(mcptools.Options{Data: mcptools.NewStore(sc.World), Fault: fm,
		FaultDelay: 300 * time.Millisecond})
	_, remote := mcptools.InMemory(nil, srv)

	opt := audit.Options{Injected: faults}
	repOut := audit.Run(audit.New(r.RF), ctx, &sc.Claim, sc.InvoiceText, sc.SupportText,
		audit.WrapMCP(remote), rv, opt)

	c.Got = repOut.Result.Decision
	c.Hits = repOut.Result.Hits
	c.Events = len(repOut.Events)
	c.Unverified = repOut.Result.Unverified
	c.Warnings = repOut.Warnings
	c.Passed = contains(accept, c.Got)
	if !c.Passed {
		c.FailWhy = fmt.Sprintf("结论为 %s，期望 %s", c.Got, strings.Join(accept, "/"))
	}
	// 额外要求：事件流至少要覆盖五个阶段，否则过程不可观测。
	stages := map[audit.Stage]bool{}
	for _, ev := range repOut.Events {
		stages[ev.Stage] = true
	}
	for _, st := range []audit.Stage{audit.StagePlan, audit.StageExtract, audit.StageTools, audit.StageAggregate} {
		if !stages[st] {
			c.Passed = false
			c.FailWhy = appendWhy(c.FailWhy, "事件流缺少阶段 "+string(st))
		}
	}
	return c
}

func appendWhy(old, add string) string {
	if old == "" {
		return add
	}
	return old + "；" + add
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// Markdown 生成人读的彩排记录，便于贴进交付文档。
func (r *Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 现场彩排记录\n\n时间：%s\n\n", r.AtText)
	fmt.Fprintf(&b, "结果：%d/%d 通过\n\n", r.Passed, r.Total)
	b.WriteString("| 场景 | 单据 | 结论 | 期望 | 事件数 | 耗时 | 结果 |\n|---|---|---|---|---|---|---|\n")
	for _, c := range r.Cases {
		res := "通过"
		if !c.Passed {
			res = "未通过：" + c.FailWhy
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %d | %dms | %s |\n",
			c.Name, c.CaseID, c.Got, strings.Join(c.Want, "/"), c.Events, c.ElapsedMS, res)
	}
	b.WriteString("\n## 每次彩排说明了什么\n")
	for _, c := range r.Cases {
		fmt.Fprintf(&b, "\n**%s**（%s）\n\n- 目的：%s\n", c.Name, c.CaseID, c.Purpose)
		if len(c.Unverified) > 0 {
			fmt.Fprintf(&b, "- 未核验：%s\n", strings.Join(c.Unverified, "、"))
		}
		if len(c.Warnings) > 0 {
			fmt.Fprintf(&b, "- 降级：%s\n", strings.Join(c.Warnings, "；"))
		}
		fmt.Fprintf(&b, "- 命中：%v\n", c.Hits)
	}
	return b.String()
}

// Write 把 JSON 与 Markdown 两种格式写到 dir。
func (r *Report) Write(dir string) (string, string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	jp := filepath.Join(dir, "rehearsal.json")
	mp := filepath.Join(dir, "rehearsal.md")
	if err := schema.WriteJSON(jp, r); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(mp, []byte(r.Markdown()), 0o644); err != nil {
		return "", "", err
	}
	return jp, mp, nil
}
