package audit_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"reimburse/internal/audit"
	"reimburse/internal/llm"
	"reimburse/internal/mcptools"
	"reimburse/internal/schema"
	"reimburse/rules"
)

// 端到端：用例 -> MCP 服务端（内存传输）-> MCP 客户端 -> 规则引擎。
// 这一层证明"工具是真 MCP 工具"，而不是本地函数调用包装出来的假连接。

// seedLLM 把用例的标准答案当作"模型抽取成功的字段"，让测试聚焦在工具链路上。
type seedLLM struct {
	fields map[string]llm.FieldValue
	fail   *llm.SchemaIssue
}

func (s *seedLLM) ExtractWith(ctx context.Context, provider string, src llm.Source, onCall func(*llm.Response)) (*llm.Extraction, *llm.SchemaIssue) {
	if s.fail != nil {
		return nil, s.fail
	}
	return &llm.Extraction{Provider: provider, Model: "seed", Fields: s.fields, Attempts: 1}, nil
}

// mcpExplorer 起一个内存 MCP 服务端，返回带记录的 Explorer。
func mcpExplorer(t *testing.T, c *schema.Case) *audit.RemoteSpy {
	t.Helper()
	srv := mcptools.NewServer(mcptools.Options{Data: mcptools.NewStore(c.World)})
	_, client := mcptools.InMemory(t, srv)
	return &audit.RemoteSpy{Inner: audit.WrapMCP(client)}
}

// 全部用例走 MCP 工具链路，结论必须与标准答案一致。
func TestMCPServerDrivesEngineOnAllCases(t *testing.T) {
	rf, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	cases, err := loadCases(t)
	if err != nil {
		t.Fatal(err)
	}
	eng := audit.New(rf)
	cfg := llm.DefaultConfig()
	for _, c := range cases {
		spy := mcpExplorer(t, c)
		rv := &audit.Reviewer{LLM: &seedLLM{fields: audit.InputFromCase(c).Fields}, Cfg: &cfg}
		seedFaults(c, rv)
		seedFaults(c, rv)
		rep := eng.Run(context.Background(), &c.Claim, c.InvoiceText, c.SupportText, spy, rv, audit.Options{})
		if rep.Result.Decision != c.Truth.Decision {
			t.Errorf("%s 结论不一致：MCP 链路=%s 标注=%s 命中=%v",
				c.ID, rep.Result.Decision, c.Truth.Decision, rep.Result.Hits)
		}
		if rep.Result.ApprovedFen != c.Truth.ApprovedFen {
			t.Errorf("%s 核准金额不一致：MCP=%d 标注=%d", c.ID, rep.Result.ApprovedFen, c.Truth.ApprovedFen)
		}
	}
}

// 每次工具调用都必须留痕：工具名、入参、状态、返回原始文本。
func TestMCPToolCallsAreRecorded(t *testing.T) {
	cases, _ := loadCases(t)
	var c *schema.Case
	for _, x := range cases {
		if len(x.Truth.Hits) == 0 {
			c = x
			break
		}
	}
	if c == nil {
		t.Skip("没有干净用例")
	}
	spy := mcpExplorer(t, c)
	cfg := llm.DefaultConfig()
	rv := &audit.Reviewer{LLM: &seedLLM{fields: audit.InputFromCase(c).Fields}, Cfg: &cfg}
	seedFaults(c, rv)
	eng := audit.New(mustRules(t))
	rep := eng.Run(context.Background(), &c.Claim, c.InvoiceText, c.SupportText, spy, rv, audit.Options{})

	if len(spy.History) != 3 {
		t.Fatalf("应调用 3 个工具，实际 %d 次: %+v", len(spy.History), spy.History)
	}
	want := map[string]bool{"verify_invoice": false, "check_company": false, "check_history": false}
	for _, ev := range spy.History {
		want[ev.Tool] = true
		if ev.Status != audit.ToolOK {
			t.Errorf("%s 应成功: %+v", ev.Tool, ev)
		}
		if ev.Raw == "" {
			t.Errorf("%s 没有留下原始返回", ev.Tool)
		}
		if ev.Args == "" {
			t.Errorf("%s 没有记录入参", ev.Tool)
		}
		if ev.Duration < 0 {
			t.Errorf("%s 没有记录耗时", ev.Tool)
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("工具 %s 未被调用", k)
		}
	}
	// 报告里必须带有工具返回，界面靠它展示。
	for name, tr := range rep.Tools {
		if tr.Status == audit.ToolOK && tr.Raw == "" {
			t.Errorf("报告里工具 %s 缺少原始返回", name)
		}
	}
}

// 工具层故障与模型层故障必须能叠加：一个转人工的结论不能因为另一个恢复而变成通过。
// seedFaults 与既有引擎测试保持一致：用例里的预置故障通过演示开关注入，
// 否则"真实故障"会消失，结论也会从转人工变成可直接判定。
func seedFaults(c *schema.Case, rv *audit.Reviewer) {
	rv.ToolFaults = map[string]audit.ToolStatus{}
	for _, f := range c.World.Faults {
		st := audit.ToolFailed
		if f.Mode == schema.FaultTimeout {
			st = audit.ToolTimeout
		}
		rv.ToolFaults[f.Tool] = st
	}
}

func TestMCPToolFaultStillManualReview(t *testing.T) {
	cases, _ := loadCases(t)
	var c *schema.Case
	for _, x := range cases {
		if len(x.Truth.Hits) == 0 {
			c = x
			break
		}
	}
	srv := mcptools.NewServer(mcptools.Options{Data: mcptools.NewStore(c.World),
		Fault: map[string]string{mcptools.ToolVerifyInvoice: "timeout"}, FaultDelay: 50 * time.Millisecond})
	_, client := mcptools.InMemory(t, srv)
	spy := &audit.RemoteSpy{Inner: audit.WrapMCP(client)}
	cfg := llm.DefaultConfig()
	rv := &audit.Reviewer{LLM: &seedLLM{fields: audit.InputFromCase(c).Fields}, Cfg: &cfg}
	seedFaults(c, rv)
	eng := audit.New(mustRules(t))
	rep := eng.Run(context.Background(), &c.Claim, c.InvoiceText, c.SupportText, spy, rv, audit.Options{})
	if rep.Result.Decision == rules.Approve {
		t.Fatal("查验超时不得通过")
	}
	if !hasHit(rep.Result.Hits, "R15-a") {
		t.Errorf("应命中 R15-a，实际 %v", rep.Result.Hits)
	}
}

// loadCases 读测试集。
func loadCases(t *testing.T) ([]*schema.Case, error) {
	t.Helper()
	dir := os.Getenv("REIMBURSE_CASES")
	if dir == "" {
		dir = "../../data"
	}
	return schema.LoadAll(dir)
}

func mustRules(t *testing.T) *rules.File {
	t.Helper()
	rf, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	return rf
}

var _ = errors.Is
var _ = strings.TrimSpace

func hasHit(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

var _ = errors.Is
var _ = strings.TrimSpace
