package audit_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"reimburse/internal/audit"
	"reimburse/internal/llm"
	"reimburse/internal/mcptools"
	"reimburse/internal/schema"
	"reimburse/rules"
)

// 端到端联调测试：用本地假供应商（真 HTTP、真 JSON 协议）替代 DeepSeek 与 Claude，
// 打通 真实 HTTP 请求 → 适配器解析 → Schema 校验 → 双模型比对 → MCP 工具 → 规则判定。
//
// 为什么需要它：开发环境没有密钥，真实链路一直没跑过。这个测试把"协议层"也纳入验证，
// 能覆盖端点拼接、鉴权头、结构化输出字段、响应解析、错误分类等平时只有上线才暴露的问题。
// 它不能替代真实模型联调（模型行为无法用假服务模拟），但能保证链路本身是通的。

// fakeProvider 模拟一家 OpenAI 兼容或 Anthropic 兼容服务。
type fakeProvider struct {
	t        *testing.T
	kind     string // openai / anthropic
	fields   map[string]string
	conf     float64
	mu       sync.Mutex
	calls    int
	lastBody map[string]any
	// failWith 非 0 时返回该 HTTP 状态码。
	failWith int
	// badJSON 为真时返回无法解析成 Schema 的文本。
	badJSON bool
}

func (f *fakeProvider) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls++
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.lastBody = body
		fail, bad := f.failWith, f.badJSON
		f.mu.Unlock()

		if fail != 0 {
			w.WriteHeader(fail)
			fmt.Fprintf(w, `{"error":{"type":"invalid_request_error","message":"fake %d"}}`, fail)
			return
		}

		var text string
		if bad {
			text = "抱歉，我看不清这张票。"
		} else {
			text = f.extractJSON()
		}
		switch f.kind {
		case "openai":
			if r.URL.Path != "/v1/chat/completions" {
				http.NotFound(w, r)
				return
			}
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(401)
				return
			}
			// 结构化输出模式下必须带 response_format。
			if body["response_format"] == nil {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":{"message":"json mode not requested"}}`)
				return
			}
			out := map[string]any{
				"choices": []any{map[string]any{
					"message":       map[string]any{"content": text},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{"prompt_tokens": 1180, "completion_tokens": 240, "prompt_cache_hit_tokens": 900},
			}
			_ = json.NewEncoder(w).Encode(out)
		case "anthropic":
			if r.URL.Path != "/v1/messages" {
				http.NotFound(w, r)
				return
			}
			if r.Header.Get("x-api-key") == "" || r.Header.Get("anthropic-version") == "" {
				w.WriteHeader(401)
				return
			}
			// system 必须是顶层字段，结构化输出走 output_config.format。
			if _, ok := body["system"]; !ok {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"missing system"}}`)
				return
			}
			if body["output_config"] == nil {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"missing output_config"}}`)
				return
			}
			out := map[string]any{
				"content":     []any{map[string]any{"type": "text", "text": text}},
				"stop_reason": "end_turn",
				"usage":       map[string]any{"input_tokens": 1180, "output_tokens": 240},
			}
			_ = json.NewEncoder(w).Encode(out)
		}
	})
}

func (f *fakeProvider) extractJSON() string {
	fields := map[string]any{}
	for k, v := range f.fields {
		conf := f.conf
		if v == "" {
			// 读不出来的字段，真实模型会给出低置信度而不是满分。
			conf = 0.2
		}
		fields[k] = map[string]any{"value": v, "confidence": conf}
	}
	b, _ := json.Marshal(map[string]any{"fields": fields, "notes": ""})
	return string(b)
}

// fieldsFrom 把用例真值转成字段字典。
func fieldsFrom(c *schema.Case) map[string]string {
	inv := c.Truth.Invoice
	un := map[string]bool{}
	for _, u := range c.Truth.Unreadable {
		un[u] = true
	}
	all := map[string]string{
		llm.FInvoiceNo:   inv.InvoiceNo,
		llm.FIssueDate:   inv.IssueDate,
		llm.FTotalAmount: fmt.Sprintf("%d.%02d", inv.TotalFen/100, inv.TotalFen%100),
		llm.FBuyerTaxID:  inv.BuyerTaxID,
		llm.FSellerTaxID: inv.SellerTaxID,
		llm.FBuyerName:   inv.BuyerName,
		llm.FSellerName:  inv.SellerName,
		llm.FItemName:    inv.ItemName,
	}
	for k := range un {
		all[k] = ""
	}
	return all
}

func loadCase(t *testing.T, id string) *schema.Case {
	t.Helper()
	dir := os.Getenv("REIMBURSE_CASES")
	if dir == "" {
		dir = "../../data"
	}
	c, err := schema.LoadCase(filepath.Join(dir, "cases", id))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// runWith 起两个假供应商，跑完整链路。
func runWith(t *testing.T, c *schema.Case, ds, cl *fakeProvider) (*audit.Report, *audit.Reviewer) {
	t.Helper()
	dsSrv := httptest.NewServer(ds.handler())
	clSrv := httptest.NewServer(cl.handler())
	t.Cleanup(func() { dsSrv.Close(); clSrv.Close() })

	cfg := llm.DefaultConfig()
	cfg.DeepSeek.BaseURL = dsSrv.URL
	cfg.Claude.BaseURL = clSrv.URL
	cfg.DeepSeek.APIKeyEnv = "FAKE_DS_KEY"
	cfg.Claude.APIKeyEnv = "FAKE_CL_KEY"
	cfg.DeepSeek.TimeoutSec, cfg.Claude.TimeoutSec = 5, 5
	cfg.DeepSeek.MaxRetries, cfg.Claude.MaxRetries = 0, 0
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_DS_KEY", "sk-fake-deepseek")
	t.Setenv("FAKE_CL_KEY", "sk-fake-claude")

	rf, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	client := llm.NewClient(&cfg)
	rv := &audit.Reviewer{LLM: client, Client: client, Cfg: &cfg, Disabled: map[string]bool{}}
	srv := mcptools.NewServer(mcptools.Options{Data: mcptools.NewStore(c.World)})
	_, remote := mcptools.InMemory(t, srv)
	eng := audit.New(rf)
	rep := eng.Run(context.Background(), &c.Claim, c.InvoiceText, c.SupportText, audit.WrapMCP(remote), rv, audit.Options{})
	return rep, rv
}

// 两家模型都正常返回：结论必须与标准答案一致，且事件里能看到真实 token 用量。
func TestHTTPLinkEndToEnd(t *testing.T) {
	c := loadCase(t, "C01")
	ds := &fakeProvider{t: t, kind: "openai", fields: fieldsFrom(c), conf: 1}
	cl := &fakeProvider{t: t, kind: "anthropic", fields: fieldsFrom(c), conf: 1}
	rep, _ := runWith(t, c, ds, cl)

	if rep.Result.Decision != c.Truth.Decision {
		t.Fatalf("结论不一致：链路=%s 标注=%s 命中=%v", rep.Result.Decision, c.Truth.Decision, rep.Result.Hits)
	}
	if rep.Result.ApprovedFen != c.Truth.ApprovedFen {
		t.Errorf("核准金额不一致：%d vs %d", rep.Result.ApprovedFen, c.Truth.ApprovedFen)
	}
	if ds.calls != 1 || cl.calls != 1 {
		t.Errorf("两家各应调用一次，实际 deepseek=%d claude=%d", ds.calls, cl.calls)
	}
	// 真实 HTTP 下事件里应带真实 token 数与成本。
	var sawTokens bool
	for _, ev := range rep.Events {
		if ev.Kind == "llm_result" && strings.Contains(ev.Detail, "1180") {
			sawTokens = true
		}
	}
	if !sawTokens {
		t.Error("事件流里没有真实 token 用量，说明没走 HTTP 链路")
	}
	if rep.CostUSD <= 0 {
		t.Error("走了真实模型调用，成本应大于 0")
	}
	// 请求体契约：OpenAI 侧带 response_format，Anthropic 侧带 output_config 与顶层 system。
	if _, ok := ds.lastBody["response_format"]; !ok {
		t.Error("DeepSeek 请求缺少 response_format")
	}
	if _, ok := cl.lastBody["output_config"]; !ok {
		t.Error("Claude 请求缺少 output_config")
	}
	if _, ok := cl.lastBody["system"]; !ok {
		t.Error("Claude 请求 system 应为顶层字段")
	}
}

// Claude 挂了：降级为单模型，但结论必须转人工（关键字段无交叉校验）。
func TestHTTPLinkOneProviderDown(t *testing.T) {
	c := loadCase(t, "C01")
	ds := &fakeProvider{t: t, kind: "openai", fields: fieldsFrom(c), conf: 1}
	cl := &fakeProvider{t: t, kind: "anthropic", fields: fieldsFrom(c), failWith: 500}
	rep, _ := runWith(t, c, ds, cl)

	if rep.Result.Decision == rules.Approve {
		t.Fatal("单模型可用时不得自动通过")
	}
	if !hasHit(rep.Result.Hits, "R15-b") {
		t.Errorf("应命中 R15-b（缺交叉校验），实际 %v", rep.Result.Hits)
	}
	if len(rep.Warnings) == 0 {
		t.Error("应给出降级说明")
	}
}

// 一家返回非 JSON：判为 bad_json，该模型不可用，不能把散文当成字段。
func TestHTTPLinkBadJSON(t *testing.T) {
	c := loadCase(t, "C01")
	ds := &fakeProvider{t: t, kind: "openai", fields: fieldsFrom(c), conf: 1}
	cl := &fakeProvider{t: t, kind: "anthropic", fields: fieldsFrom(c), badJSON: true}
	rep, _ := runWith(t, c, ds, cl)
	if rep.Result.Decision == rules.Approve {
		t.Fatal("一个模型返回非 JSON 时不得自动通过")
	}
	var sawBadJSON bool
	for _, ev := range rep.Events {
		if ev.Kind == "warn" && strings.Contains(ev.Title, "Claude") {
			sawBadJSON = true
		}
	}
	if !sawBadJSON {
		t.Error("事件流里应看到 Claude 解析失败")
	}
}

// 两家字段不一致：该字段作废，不取任何一边。
func TestHTTPLinkDisagreement(t *testing.T) {
	c := loadCase(t, "C01")
	a := fieldsFrom(c)
	b := fieldsFrom(c)
	b[llm.FTotalAmount] = "9999.99"
	ds := &fakeProvider{t: t, kind: "openai", fields: a, conf: 1}
	cl := &fakeProvider{t: t, kind: "anthropic", fields: b, conf: 1}
	rep, _ := runWith(t, c, ds, cl)

	if rep.Extract == nil {
		t.Fatal("缺少抽取结果")
	}
	if !containsStr(rep.Extract.Disagree, llm.FTotalAmount) {
		t.Errorf("金额不一致应被识别，实际 %v", rep.Extract.Disagree)
	}
	if rep.Extract.Fields[llm.FTotalAmount].Value != "" {
		t.Errorf("不一致字段不应取任何一边的值，实际 %q", rep.Extract.Fields[llm.FTotalAmount].Value)
	}
	if rep.Result.Decision == rules.Approve {
		t.Fatal("关键字段不一致时不得自动通过")
	}
}

// 低置信度：字段作废，转人工。
func TestHTTPLinkLowConfidence(t *testing.T) {
	c := loadCase(t, "C01")
	ds := &fakeProvider{t: t, kind: "openai", fields: fieldsFrom(c), conf: 0.55}
	cl := &fakeProvider{t: t, kind: "anthropic", fields: fieldsFrom(c), conf: 0.55}
	rep, _ := runWith(t, c, ds, cl)
	if rep.Result.Decision == rules.Approve {
		t.Fatal("置信度普遍不足时不得自动通过")
	}
	if !hasHit(rep.Result.Hits, "R15-b") {
		t.Errorf("应命中 R15-b，实际 %v", rep.Result.Hits)
	}
}

// 全部 60 条用例走真实 HTTP 链路，结论必须与标准答案一致。
// 这条测试把"协议层 + 规则层 + 工具层"整条链路一起锁死。
func TestHTTPLinkAllCases(t *testing.T) {
	dir := os.Getenv("REIMBURSE_CASES")
	if dir == "" {
		dir = "../../data"
	}
	cases, err := schema.LoadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	rf, _ := rules.Load()
	bad := 0
	for _, c := range cases {
		ds := &fakeProvider{t: t, kind: "openai", fields: fieldsFrom(c), conf: 1}
		cl := &fakeProvider{t: t, kind: "anthropic", fields: fieldsFrom(c), conf: 1}
		dsSrv := httptest.NewServer(ds.handler())
		clSrv := httptest.NewServer(cl.handler())

		cfg := llm.DefaultConfig()
		cfg.DeepSeek.BaseURL, cfg.Claude.BaseURL = dsSrv.URL, clSrv.URL
		cfg.DeepSeek.APIKeyEnv, cfg.Claude.APIKeyEnv = "FAKE_DS_KEY", "FAKE_CL_KEY"
		cfg.DeepSeek.TimeoutSec, cfg.Claude.TimeoutSec = 5, 5
		cfg.DeepSeek.MaxRetries, cfg.Claude.MaxRetries = 0, 0
		_ = cfg.Normalize()
		t.Setenv("FAKE_DS_KEY", "k")
		t.Setenv("FAKE_CL_KEY", "k")

		// 用例里的预置故障通过演示开关注入，与引擎测试保持一致。
		rv := &audit.Reviewer{LLM: llm.NewClient(&cfg), Cfg: &cfg, Disabled: map[string]bool{}, ToolFaults: map[string]audit.ToolStatus{}}
		for _, f := range c.World.Faults {
			st := audit.ToolFailed
			if f.Mode == schema.FaultTimeout {
				st = audit.ToolTimeout
			}
			rv.ToolFaults[f.Tool] = st
		}
		srv := mcptools.NewServer(mcptools.Options{Data: mcptools.NewStore(c.World)})
		_, remote := mcptools.InMemory(t, srv)
		rep := audit.New(rf).Run(context.Background(), &c.Claim, c.InvoiceText, c.SupportText,
			audit.WrapMCP(remote), rv, audit.Options{})

		if rep.Result.Decision != c.Truth.Decision {
			t.Errorf("%s %s：链路=%s 标注=%s 命中=%v", c.ID, c.Truth.Title, rep.Result.Decision, c.Truth.Decision, rep.Result.Hits)
			bad++
		}
		if rep.Result.ApprovedFen != c.Truth.ApprovedFen {
			t.Errorf("%s 核准金额：%d vs %d", c.ID, rep.Result.ApprovedFen, c.Truth.ApprovedFen)
			bad++
		}
		dsSrv.Close()
		clSrv.Close()
		if bad > 5 {
			t.Fatal("失败过多，提前停止")
		}
	}
}

func containsStr(xs []string, x string) bool { return hasHit(xs, x) }
