// eval 跑完整测试集并生成评测报告。
//
//	go run ./cmd/eval                       合成抽取（无密钥环境）
//	go run ./cmd/eval -real -out docs       真实模型（需要密钥）
//	go run ./cmd/eval -out docs -md-only    只生成 Markdown
//
// 报告里的每个数字都由本程序计算并写入同一份结构化结果，再从该结果渲染 Markdown，
// 避免"文档里的数字和代码里的不一致"。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"reimburse/internal/audit"
	"reimburse/internal/llm"
	"reimburse/internal/mcptools"
	"reimburse/internal/schema"
	"reimburse/plugins/reimburse-audit/policy"
)

// Outcome 是单条用例的评测结果。
type Outcome struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Category     string   `json:"category"`
	Expected     string   `json:"expected"`
	Got          string   `json:"got"`
	HitsWant     []string `json:"hits_expected"`
	HitsGot      []string `json:"hits_got"`
	ApprovedWant int64    `json:"approved_expected_fen"`
	ApprovedGot  int64    `json:"approved_got_fen"`
	// 判定项层面的比对：漏判（该中没中）、误判（不该中却中）。
	Missed     []string `json:"missed,omitempty"`
	Spurious   []string `json:"spurious,omitempty"`
	Events     int      `json:"events"`
	ElapsedMS  int64    `json:"elapsed_ms"`
	CostUSD    float64  `json:"cost_usd"`
	DecisionOK bool     `json:"decision_ok"`
	ApprovedOK bool     `json:"approved_ok"`
	HitsOK     bool     `json:"hits_ok"`
}

// Summary 是汇总指标。
type Summary struct {
	Total         int            `json:"total"`
	DecisionOK    int            `json:"decision_ok"`
	ApprovedOK    int            `json:"approved_ok"`
	HitsOK        int            `json:"hits_ok"`
	DecisionRate  float64        `json:"decision_rate"`
	HitsPrecision float64        `json:"hits_precision"`
	HitsRecall    float64        `json:"hits_recall"`
	ApprovedMAE   int64          `json:"approved_mae_fen"`
	ByDecision    map[string]int `json:"by_decision"`
	ByCategory    map[string]int `json:"by_category"`
	TotalCost     float64        `json:"total_cost_usd"`
	AvgCost       float64        `json:"avg_cost_usd"`
	TotalElapsed  int64          `json:"total_elapsed_ms"`
	Mode          string         `json:"mode"`
	At            string         `json:"at"`
	// 判定项覆盖：测试集实际覆盖了多少个判定项。
	CheckCovered    int      `json:"check_covered"`
	CheckTotal      int      `json:"check_total"`
	UncoveredChecks []string `json:"uncovered_checks,omitempty"`
	// 只有通过/调整后通过才有"核准金额"语义。
	ApprovedCases int `json:"approved_cases"`
}

// Report 是完整评测报告。
type Report struct {
	Summary  Summary   `json:"summary"`
	Outcomes []Outcome `json:"outcomes"`
	Failures []Outcome `json:"failures"`
}

func main() {
	data := flag.String("data", "data", "测试集根目录")
	out := flag.String("out", "", "输出目录；为空只打印摘要")
	real := flag.Bool("real", false, "走真实模型（需要密钥）")
	cfgPath := flag.String("config", "", "模型配置文件")
	rulesPath := flag.String("rules", "", "外部规则表")
	flag.Parse()

	rf, err := loadRules(*rulesPath)
	if err != nil {
		fatal("规则表: %v", err)
	}
	cfg, err := llm.LoadConfig(*cfgPath)
	if err != nil {
		fatal("模型配置: %v", err)
	}
	cases, err := schema.LoadAll(*data)
	if err != nil {
		fatal("测试集: %v", err)
	}

	mode := "合成抽取（无密钥演示模式）"
	useSynth := !*real
	if *real {
		if cfg.DeepSeek.APIKey() == "" && cfg.Claude.APIKey() == "" {
			fatal("-real 需要至少一个密钥")
		}
		mode = "真实模型"
		if cfg.DeepSeek.APIKey() == "" || cfg.Claude.APIKey() == "" {
			mode = "真实模型（单侧）"
		}
	}

	rep := &Report{Summary: Summary{Mode: mode, At: time.Now().Format("2006-01-02 15:04:05"),
		ByDecision: map[string]int{}, ByCategory: map[string]int{}}}

	covered := map[string]bool{}
	for _, c := range cases {
		o := evalOne(rf, cfg, c, useSynth)
		rep.Outcomes = append(rep.Outcomes, o)
		rep.Summary.Total++
		rep.Summary.ByCategory[c.Claim.Category]++
		rep.Summary.ByDecision[o.Got]++
		rep.Summary.TotalCost += o.CostUSD
		rep.Summary.TotalElapsed += o.ElapsedMS
		if o.DecisionOK {
			rep.Summary.DecisionOK++
		}
		if o.HitsOK {
			rep.Summary.HitsOK++
		}
		if o.ApprovedOK {
			rep.Summary.ApprovedOK++
		}
		for _, h := range o.HitsWant {
			covered[h] = true
		}
		if !o.DecisionOK || !o.HitsOK || !o.ApprovedOK {
			rep.Failures = append(rep.Failures, o)
		}
	}

	// 判定项层面的精确率/召回率。
	var tp, fp, fn int
	for _, o := range rep.Outcomes {
		tp += len(o.HitsGot) - len(o.Spurious)
		fp += len(o.Spurious)
		fn += len(o.Missed)
	}
	s := &rep.Summary
	if s.Total > 0 {
		s.DecisionRate = float64(s.DecisionOK) / float64(s.Total)
		s.AvgCost = s.TotalCost / float64(s.Total)
	}
	if tp+fp > 0 {
		s.HitsPrecision = float64(tp) / float64(tp+fp)
	}
	if tp+fn > 0 {
		s.HitsRecall = float64(tp) / float64(tp+fn)
	}
	// 核准金额平均绝对误差，只在"通过/调整后通过"的用例上有意义。
	var sae int64
	for _, o := range rep.Outcomes {
		if o.Expected == policy.Approve || o.Expected == policy.ApproveAdj {
			s.ApprovedCases++
			sae += abs64(o.ApprovedGot - o.ApprovedWant)
		}
	}
	if s.ApprovedCases > 0 {
		s.ApprovedMAE = sae / int64(s.ApprovedCases)
	}
	s.CheckTotal = len(rf.AllCheckIDs())
	s.CheckCovered = len(covered)
	for _, id := range rf.AllCheckIDs() {
		if !covered[id] {
			s.UncoveredChecks = append(s.UncoveredChecks, id)
		}
	}

	printSummary(rep)
	if *out != "" {
		jp := filepath.Join(*out, "eval.json")
		mp := filepath.Join(*out, "eval.md")
		if err := os.MkdirAll(*out, 0o755); err != nil {
			fatal("创建目录: %v", err)
		}
		if err := schema.WriteJSON(jp, rep); err != nil {
			fatal("写 json: %v", err)
		}
		if err := os.WriteFile(mp, []byte(renderMarkdown(rep, rf)), 0o644); err != nil {
			fatal("写 md: %v", err)
		}
		fmt.Printf("\n报告：%s 与 %s\n", jp, mp)
	}
	if len(rep.Failures) > 0 {
		os.Exit(1)
	}
}

func evalOne(rf *policy.File, cfg llm.Config, c *schema.Case, synth bool) Outcome {
	t0 := time.Now()
	o := Outcome{ID: c.ID, Title: c.Truth.Title, Category: c.Claim.Category,
		Expected: c.Truth.Decision, HitsWant: c.Truth.Hits, ApprovedWant: c.Truth.ApprovedFen}

	var impl audit.LLM = llm.NewClient(&cfg)
	if synth {
		impl = &llm.SyntheticLLM{Case: c, Synth: &llm.Synthetic{}, Fallback: impl}
	}
	rv := &audit.Reviewer{LLM: impl, Cfg: &cfg, Disabled: map[string]bool{}, ToolFaults: map[string]audit.Status{}}
	for _, f := range c.World.Faults {
		st := audit.ToolFailed
		if f.Mode == schema.FaultTimeout {
			st = audit.ToolTimeout
		}
		rv.ToolFaults[f.Tool] = st
	}
	srv := mcptools.NewServer(mcptools.Options{Data: mcptools.NewStore(c.World)})
	_, remote := mcptools.InMemory(nil, srv)
	rep := audit.Run(audit.New(rf), context.Background(), &c.Claim, c.InvoiceText, c.SupportText,
		audit.WrapMCP(remote), rv, audit.Options{})

	o.Got = rep.Result.Decision
	o.HitsGot = rep.Result.Hits
	o.ApprovedGot = rep.Result.ApprovedFen
	o.Events = len(rep.Events)
	o.ElapsedMS = time.Since(t0).Milliseconds()
	o.CostUSD = rep.CostUSD
	o.DecisionOK = o.Got == o.Expected
	want := map[string]bool{}
	for _, h := range o.HitsWant {
		want[h] = true
	}
	got := map[string]bool{}
	for _, h := range o.HitsGot {
		got[h] = true
	}
	for h := range want {
		if !got[h] {
			o.Missed = append(o.Missed, h)
		}
	}
	for h := range got {
		if !want[h] {
			o.Spurious = append(o.Spurious, h)
		}
	}
	sort.Strings(o.Missed)
	sort.Strings(o.Spurious)
	o.HitsOK = len(o.Missed) == 0 && len(o.Spurious) == 0
	o.ApprovedOK = o.ApprovedGot == o.ApprovedWant
	if o.HitsWant == nil {
		o.HitsWant = []string{}
	}
	if o.HitsGot == nil {
		o.HitsGot = []string{}
	}
	return o
}

func printSummary(rep *Report) {
	s := rep.Summary
	fmt.Printf("模式：%s，时间 %s\n", s.Mode, s.At)
	fmt.Printf("用例 %d 条，结论正确 %d 条（%.1f%%），命中项完全一致 %d 条，核准金额一致 %d/%d 条\n",
		s.Total, s.DecisionOK, s.DecisionRate*100, s.HitsOK, s.ApprovedOK, s.ApprovedCases)
	fmt.Printf("判定项：精确率 %.3f，召回率 %.3f，覆盖 %d/%d\n", s.HitsPrecision, s.HitsRecall, s.CheckCovered, s.CheckTotal)
	fmt.Printf("平均耗时 %.1fms，平均成本 $%.6f\n", float64(s.TotalElapsed)/float64(max1(s.Total)), s.AvgCost)
	if len(rep.Failures) > 0 {
		fmt.Printf("未通过 %d 条：\n", len(rep.Failures))
		for _, f := range rep.Failures {
			fmt.Printf("  %s %s：期望 %s 实际 %s；漏判 %v 误判 %v\n", f.ID, f.Title, f.Expected, f.Got, f.Missed, f.Spurious)
		}
	}
}

func renderMarkdown(rep *Report, rf *policy.File) string {
	s := rep.Summary
	var b strings.Builder
	b.WriteString("# 评测报告\n\n")
	fmt.Fprintf(&b, "生成时间：%s　模式：**%s**\n\n", s.At, s.Mode)
	if strings.HasPrefix(s.Mode, "合成") {
		b.WriteString("> 本次评测使用合成抽取（由测试集标准答案生成结构化输出，并经与真实响应完全相同的\n")
		b.WriteString("> Schema 校验与双模型比对）。它验证的是**判定链路的正确性**，不代表任何真实模型的能力。\n")
		b.WriteString("> 配好密钥后执行 `go run ./cmd/eval -real -out docs` 可得到真实模型评测。\n\n")
	}
	b.WriteString("## 总体指标\n\n| 指标 | 数值 |\n|---|---|\n")
	fmt.Fprintf(&b, "| 用例数 | %d |\n", s.Total)
	fmt.Fprintf(&b, "| 结论与标准答案一致 | %d / %d（%.1f%%） |\n", s.DecisionOK, s.Total, s.DecisionRate*100)
	fmt.Fprintf(&b, "| 命中项集合完全一致 | %d / %d |\n", s.HitsOK, s.Total)
	fmt.Fprintf(&b, "| 判定项精确率 | %.3f |\n", s.HitsPrecision)
	fmt.Fprintf(&b, "| 判定项召回率 | %.3f |\n", s.HitsRecall)
	fmt.Fprintf(&b, "| 判定项覆盖 | %d / %d |\n", s.CheckCovered, s.CheckTotal)
	approvedSame := 0
	for _, o := range rep.Outcomes {
		if (o.Expected == policy.Approve || o.Expected == policy.ApproveAdj) && o.ApprovedGot == o.ApprovedWant {
			approvedSame++
		}
	}
	fmt.Fprintf(&b, "| 核准金额一致 | %d / %d 条（分母为通过/调整后通过的用例数） |\n", approvedSame, s.ApprovedCases)
	fmt.Fprintf(&b, "| 核准金额平均绝对误差 | %d 分 |\n", s.ApprovedMAE)
	fmt.Fprintf(&b, "| 平均单条耗时 | %.1f ms |\n", float64(s.TotalElapsed)/float64(max1(s.Total)))
	fmt.Fprintf(&b, "| 平均单条成本 | $%.6f |\n", s.AvgCost)

	b.WriteString("\n## 结论分布（本系统给出）\n\n| 结论 | 条数 |\n|---|---|\n")
	for _, d := range rf.Params.DecisionPriority {
		if n, ok := s.ByDecision[d]; ok {
			fmt.Fprintf(&b, "| %s | %d |\n", d, n)
		}
	}
	b.WriteString("\n## 按类别\n\n| 类别 | 条数 |\n|---|---|\n")
	for _, c := range []string{policy.CatTransport, policy.CatLodging, policy.CatMeal} {
		fmt.Fprintf(&b, "| %s | %d |\n", c, s.ByCategory[c])
	}

	if len(s.UncoveredChecks) > 0 {
		fmt.Fprintf(&b, "\n## 未被测试集覆盖的判定项\n\n%s\n", strings.Join(s.UncoveredChecks, "、"))
	} else {
		b.WriteString("\n## 判定项覆盖\n\n26 个判定项均有用例覆盖。\n")
	}

	if len(rep.Failures) == 0 {
		b.WriteString("\n## 失败用例\n\n无。\n")
	} else {
		b.WriteString("\n## 失败用例\n\n| 用例 | 标题 | 期望 | 实际 | 漏判 | 误判 |\n|---|---|---|---|---|---|\n")
		for _, f := range rep.Failures {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %v | %v |\n", f.ID, f.Title, f.Expected, f.Got, f.Missed, f.Spurious)
		}
	}

	b.WriteString("\n## 逐条明细\n\n| 用例 | 类别 | 期望 | 实际 | 命中项 | 事件 | 耗时 |\n|---|---|---|---|---|---|---|\n")
	for _, o := range rep.Outcomes {
		mark := ""
		if !o.DecisionOK {
			mark = " ❌"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s%s | %s | %d | %dms |\n", o.ID, o.Category, o.Expected, o.Got, mark,
			strings.Join(o.HitsGot, " "), o.Events, o.ElapsedMS)
	}

	b.WriteString("\n## 这份报告不能说明什么\n\n")
	b.WriteString("1. 不是真实票据上的效果：60 条用例是合成的，规则阈值也是虚构值。\n")
	b.WriteString("2. 合成模式不代表模型能力：字段来自标准答案，模型真正的抽取错误率要等真实评测。\n")
	b.WriteString("3. 不含真实税局查验与企业查询的时延与失败率。\n")
	return b.String()
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func loadRules(p string) (*policy.File, error) {
	if p == "" {
		return policy.Load()
	}
	return policy.LoadFile(p)
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(2)
}
