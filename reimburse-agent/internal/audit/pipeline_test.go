package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"reimburse/internal/llm"
	"reimburse/internal/schema"
	"reimburse/rules"
)

// 编排层测试：用假模型和假工具跑通全链路，验证"任何一处失败都不会落成通过"。

type fakeLLM struct {
	fields map[string]llm.FieldValue // 两个模型返回相同字段
	fail   map[string]llm.SchemaIssue
}

func (f *fakeLLM) ExtractWith(ctx context.Context, provider string, src llm.Source, onCall func(*llm.Response)) (*llm.Extraction, *llm.SchemaIssue) {
	if is, ok := f.fail[provider]; ok {
		return nil, &is
	}
	return &llm.Extraction{Provider: provider, Model: "fake", Fields: f.fields,
		Usage: llm.Usage{InputTokens: 1000, OutputTokens: 200}, LatencyMS: 5, Attempts: 1}, nil
}

type fakeExplorer struct {
	invoices  []schema.RegInvoice
	companies []schema.Company
	history   []schema.HistRecord
	faults    map[string]schema.Fault // 工具名 -> 该键的故障
	calls     []string
}

func (f *fakeExplorer) st(tool, key string) ToolStatus {
	if ft, ok := f.faults[tool]; ok && ft.Key == key {
		if ft.Mode == schema.FaultTimeout {
			return ToolTimeout
		}
		return ToolFailed
	}
	return ""
}

func (f *fakeExplorer) VerifyInvoice(ctx context.Context, no string) ToolResult {
	f.calls = append(f.calls, "verify_invoice")
	if st := f.st("verify_invoice", no); st != "" {
		return ToolResult{Status: st, Detail: "注入故障"}
	}
	return ToolResult{Status: ToolOK, Invoices: f.invoices}
}
func (f *fakeExplorer) CheckCompany(ctx context.Context, taxID string) ToolResult {
	f.calls = append(f.calls, "check_company")
	if st := f.st("check_company", taxID); st != "" {
		return ToolResult{Status: st, Detail: "注入故障"}
	}
	return ToolResult{Status: ToolOK, Companies: f.companies}
}
func (f *fakeExplorer) CheckHistory(ctx context.Context, q HistoryQuery) ToolResult {
	f.calls = append(f.calls, "check_history")
	if st := f.st("check_history", q.InvoiceNo); st != "" {
		return ToolResult{Status: st, Detail: "注入故障"}
	}
	return ToolResult{Status: ToolOK, History: f.history}
}

func fieldsOf(c *schema.Case) map[string]llm.FieldValue {
	in := InputFromCase(c)
	return in.Fields
}

func firstCaseWith(t *testing.T, want string) *schema.Case {
	t.Helper()
	cases, err := loadCases()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if contains(c.Truth.Hits, want) {
			return c
		}
	}
	t.Fatalf("没有找到命中 %s 的用例", want)
	return nil
}

func runCase(t *testing.T, c *schema.Case, opt Options, rv *Reviewer) *Report {
	t.Helper()
	rf, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	eng := New(rf)
	ex := &fakeExplorer{invoices: c.World.Invoices, companies: c.World.Companies, history: c.World.History,
		faults: map[string]schema.Fault{}}
	for _, ft := range c.World.Faults {
		ex.faults[ft.Tool] = ft
	}
	if rv == nil {
		cfg := llm.DefaultConfig()
		rv = &Reviewer{LLM: &fakeLLM{fields: fieldsOf(c)}, Cfg: &cfg, Client: llm.NewClient(&cfg)}
	}
	return eng.Run(context.Background(), &c.Claim, c.InvoiceText, c.SupportText, ex, rv, opt)
}

// 干净单据走全链路应通过，并留下计划、抽取、工具、判定的完整事件。
func TestRunCleanCase(t *testing.T) {
	c := firstCaseWith(t, "R01-a") // 先随便取一条，再找干净的
	cases, _ := loadCases()
	for _, x := range cases {
		if len(x.Truth.Hits) == 0 {
			c = x
			break
		}
	}
	rep := runCase(t, c, Options{}, nil)
	if rep.Result.Decision != rules.Approve {
		t.Fatalf("干净单据应通过，得到 %s；命中 %v", rep.Result.Decision, rep.Result.Hits)
	}
	var kinds = map[string]int{}
	for _, ev := range rep.Events {
		kinds[ev.Kind]++
	}
	for _, k := range []string{"llm_call", "llm_result", "tool_result", "decision"} {
		if kinds[k] == 0 {
			t.Errorf("缺少 %s 事件，过程不可观测", k)
		}
	}
	if len(rep.Plan) != 5 {
		t.Errorf("计划应有 5 步，得到 %d", len(rep.Plan))
	}
	if rep.CostUSD <= 0 {
		t.Errorf("应按 token 估算成本，得到 %v", rep.CostUSD)
	}
	if !strings.Contains(rep.Opinion, "通过") {
		t.Errorf("意见应写明结论: %s", rep.Opinion)
	}
	// 事件的 Seq 必须递增，界面按序渲染。
	for i := 1; i < len(rep.Events); i++ {
		if rep.Events[i].Seq <= rep.Events[i-1].Seq {
			t.Fatalf("事件序号未递增")
		}
	}
}

// 一个模型挂了：降级，但结论必须仍是转人工（关键字段没有交叉校验）。
func TestRunSingleModelDown(t *testing.T) {
	cases, _ := loadCases()
	c := cases[0]
	for _, x := range cases {
		if len(x.Truth.Hits) == 0 {
			c = x
			break
		}
	}
	rv := &Reviewer{LLM: &fakeLLM{fields: fieldsOf(c), fail: map[string]llm.SchemaIssue{
		llm.ProviderClaude: {Kind: llm.ErrTimeout, Detail: "timeout"},
	}}}
	rep := runCase(t, c, Options{}, rv)
	if rep.Result.Decision == rules.Approve {
		t.Fatalf("单模型可用时不得自动通过")
	}
	if !contains(rep.Result.Hits, "R15-b") {
		t.Errorf("应因缺少交叉校验命中 R15-b，实际 %v", rep.Result.Hits)
	}
	if len(rep.Warnings) == 0 {
		t.Errorf("应给出降级说明")
	}
}

// 两个模型都挂：不能调用工具，也不能通过。
func TestRunBothModelsDown(t *testing.T) {
	cases, _ := loadCases()
	c := cases[0]
	rv := &Reviewer{LLM: &fakeLLM{fields: map[string]llm.FieldValue{}, fail: map[string]llm.SchemaIssue{
		llm.ProviderDeepSeek: {Kind: llm.ErrNoKey, Detail: "no key"},
		llm.ProviderClaude:   {Kind: llm.ErrNoKey, Detail: "no key"},
	}}}
	rep := runCase(t, c, Options{}, rv)
	if rep.Result.Decision == rules.Approve {
		t.Fatalf("模型全挂不得通过")
	}
	if rep.Tools["verify_invoice"].Status != ToolSkipped {
		t.Errorf("发票号不可得时不应调用查验工具，实际 %s", rep.Tools["verify_invoice"].Status)
	}
}

// 工具故障注入：与真实故障等价，应命中 R15-a 并转人工。
func TestRunInjectedToolFault(t *testing.T) {
	cases, _ := loadCases()
	var c *schema.Case
	for _, x := range cases {
		if len(x.Truth.Hits) == 0 {
			c = x
			break
		}
	}
	rep := runCase(t, c, Options{Injected: map[string]ToolStatus{"verify_invoice": ToolTimeout}}, nil)
	if rep.Result.Decision == rules.Approve {
		t.Fatalf("查验超时不得通过")
	}
	if !contains(rep.Result.Hits, "R15-a") {
		t.Errorf("应命中 R15-a，实际 %v", rep.Result.Hits)
	}
	// 超时的查验不能当作"查无此票"去驳回。
	if contains(rep.Result.Hits, "R01-b") || contains(rep.Result.Hits, "R01-a") {
		t.Errorf("超时不应产生 R01 的判定: %v", rep.Result.Hits)
	}
	if !contains(rep.Result.Unverified, "verify_invoice") {
		t.Errorf("应标记未核验")
	}
}

// 所有用例走全链路，结论必须与标准答案一致。
func TestRunAllCasesMatchTruth(t *testing.T) {
	cases, err := loadCases()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		rep := runCase(t, c, Options{}, nil)
		if rep.Result.Decision != c.Truth.Decision {
			t.Errorf("%s 结论不一致：流水线=%s 标注=%s 命中=%v", c.ID, rep.Result.Decision, c.Truth.Decision, rep.Result.Hits)
		}
		if rep.Result.ApprovedFen != c.Truth.ApprovedFen {
			t.Errorf("%s 核准金额不一致：流水线=%d 标注=%d", c.ID, rep.Result.ApprovedFen, c.Truth.ApprovedFen)
		}
	}
}

// 意见核对：模型改动金额、编造条款，都必须被拦下。
func TestVerifyOpinion(t *testing.T) {
	rf, _ := rules.Load()
	eng := New(rf)
	c := firstCaseWith(t, "R10-a")
	rep := runCase(t, c, Options{}, nil)
	res := rep.Result
	if why := eng.verifyOpinion(res, "依据 "+res.Findings[0].CheckID+" 核准，金额 "+fen(res.ApprovedFen)); why != "" {
		t.Errorf("合法意见被误判: %s", why)
	}
	if why := eng.verifyOpinion(res, "核准金额 ¥999.99"); !strings.Contains(why, "未在判定结果中") {
		t.Errorf("编造的金额应被拦下，得到 %q", why)
	}
	if why := eng.verifyOpinion(res, "违反 R09-a 规定"); !strings.Contains(why, "未命中") {
		t.Errorf("未命中的条款应被拦下，得到 %q", why)
	}
	if why := eng.verifyOpinion(res, "依据 "+res.Findings[0].CheckID+"，标准见制度"); why != "" {
		t.Errorf("命中的条款应放行，得到 %q", why)
	}
}

// 事件可序列化：界面通过 SSE 发送的正是 Event 的 JSON。
func TestEventJSON(t *testing.T) {
	cases, _ := loadCases()
	rep := runCase(t, cases[0], Options{}, nil)
	for _, ev := range rep.Events {
		if _, err := json.Marshal(ev); err != nil {
			t.Fatalf("事件无法序列化: %v", err)
		}
	}
	var _ = fmt.Sprintf
}

// A/B 必须严格对应 DeepSeek/Claude。
// 曾经按通道到达顺序赋值，界面就出现过"关闭 Claude 却说只有 Claude 成功"的自相矛盾提示。
func TestDualResultProviderMapping(t *testing.T) {
	cases, _ := loadCases()
	c := cases[0]
	for _, x := range cases {
		if len(x.Truth.Hits) == 0 {
			c = x
			break
		}
	}
	// 关掉 DeepSeek：A 必须为空，B 必须是 Claude。
	rv := &Reviewer{LLM: &fakeLLM{fields: fieldsOf(c)}, Disabled: map[string]bool{"deepseek": true}}
	rep := runCaseWith(t, c, rv)
	if rep.Extract.A != nil || rep.Extract.IssueA == nil {
		t.Errorf("关闭 DeepSeek 后 A 应为空并带错误，实际 A=%v", rep.Extract.A)
	}
	if rep.Extract.B == nil {
		t.Fatal("Claude 应成功")
	}
	// 关掉 Claude：B 必须为空。
	rv = &Reviewer{LLM: &fakeLLM{fields: fieldsOf(c)}, Disabled: map[string]bool{"claude": true}}
	rep = runCaseWith(t, c, rv)
	if rep.Extract.B != nil || rep.Extract.IssueB == nil {
		t.Errorf("关闭 Claude 后 B 应为空并带错误")
	}
	if rep.Extract.A == nil {
		t.Fatal("DeepSeek 应成功")
	}
}

func runCaseWith(t *testing.T, c *schema.Case, rv *Reviewer) *Report {
	t.Helper()
	rf, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	ex := &fakeExplorer{invoices: c.World.Invoices, companies: c.World.Companies, history: c.World.History, faults: map[string]schema.Fault{}}
	for _, ft := range c.World.Faults {
		ex.faults[ft.Tool] = ft
	}
	return New(rf).Run(context.Background(), &c.Claim, c.InvoiceText, c.SupportText, ex, rv, Options{})
}
