package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 两家适配器的请求契约测试。
// 协议字段名写错时，表现通常是"线上 400/字段被忽略"，不会在编译期报错，
// 所以这里用本地服务器把实际请求体抓下来逐项断言。

type captured struct {
	Path   string
	Header http.Header
	Body   map[string]any
}

func captureServer(t *testing.T, resp string, out *captured) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out.Path = r.URL.Path
		out.Header = r.Header.Clone()
		out.Body = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&out.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
}

func TestDeepSeekRequestContract(t *testing.T) {
	var got captured
	srv := captureServer(t, dsBody(`{"a":1}`), &got)
	defer srv.Close()
	t.Setenv("TEST_DS_KEY", "ds-key")
	c := NewClient(testCfg(srv.URL))
	res := c.Call(t.Context(), ProviderDeepSeek, &Request{
		System:   "sys",
		Messages: []Message{{Role: "user", Content: "u"}},
		Schema:   map[string]any{"type": "object"},
	})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if got.Path != "/v1/chat/completions" {
		t.Errorf("路径应为 /v1/chat/completions，实际 %s", got.Path)
	}
	if got.Header.Get("Authorization") != "Bearer ds-key" {
		t.Errorf("鉴权头错误: %q", got.Header.Get("Authorization"))
	}
	if got.Body["model"] != "deepseek-flash" {
		t.Errorf("模型名: %v", got.Body["model"])
	}
	msgs, _ := got.Body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("system 应作为第一条 message: %v", got.Body["messages"])
	}
	if first, _ := msgs[0].(map[string]any); first["role"] != "system" {
		t.Errorf("第一条应为 system: %v", first)
	}
	rf, _ := got.Body["response_format"].(map[string]any)
	if rf["type"] != "json_object" {
		t.Errorf("应启用 JSON 输出: %v", got.Body["response_format"])
	}
	if got.Body["stream"] != false {
		t.Errorf("不应流式: %v", got.Body["stream"])
	}
}

// base_url 已经带 /v1 时不能拼成 /v1/v1/...。
func TestDeepSeekBaseURLWithV1(t *testing.T) {
	var got captured
	srv := captureServer(t, dsBody(`{"a":1}`), &got)
	defer srv.Close()
	t.Setenv("TEST_DS_KEY", "k")
	c := NewClient(testCfg(srv.URL + "/v1"))
	if res := c.Call(t.Context(), ProviderDeepSeek, &Request{Messages: []Message{{Role: "user", Content: "u"}}}); res.Err != nil {
		t.Fatal(res.Err)
	}
	if got.Path != "/v1/chat/completions" {
		t.Errorf("路径重复拼接: %s", got.Path)
	}
}

func TestDeepSeekUsageFields(t *testing.T) {
	resp := `{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1000,"completion_tokens":200,"prompt_cache_hit_tokens":800}}`
	var got captured
	srv := captureServer(t, resp, &got)
	defer srv.Close()
	t.Setenv("TEST_DS_KEY", "k")
	c := NewClient(testCfg(srv.URL))
	res := c.Call(t.Context(), ProviderDeepSeek, &Request{Messages: []Message{{Role: "user", Content: "u"}}})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	u := res.Resp.Usage
	if u.InputTokens != 1000 || u.OutputTokens != 200 || u.CacheHitIn != 800 {
		t.Errorf("用量解析错误: %+v", u)
	}
	// 成本按峰时价：1000/1e6*0.30 + 200/1e6*1.20
	ep := c.Cfg.DeepSeek
	if cost := ep.CostUSD(u.InputTokens, u.OutputTokens); cost < 0.0005 || cost > 0.0008 {
		t.Errorf("成本估算异常: %v", cost)
	}
}

func TestClaudeRequestContract(t *testing.T) {
	var got captured
	srv := captureServer(t, `{"content":[{"type":"text","text":"{}"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3}}`, &got)
	defer srv.Close()
	t.Setenv("TEST_CL_KEY", "cl-key")
	c := NewClient(testCfg(srv.URL))
	res := c.Call(t.Context(), ProviderClaude, &Request{
		System:   "sys",
		Messages: []Message{{Role: "user", Content: "u"}},
		Schema:   map[string]any{"type": "object", "properties": map[string]any{}},
	})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if got.Path != "/v1/messages" {
		t.Errorf("路径应为 /v1/messages，实际 %s", got.Path)
	}
	if got.Header.Get("x-api-key") != "cl-key" || got.Header.Get("anthropic-version") == "" {
		t.Errorf("鉴权或版本头缺失: %v", got.Header)
	}
	// 官方 Anthropic 只认 x-api-key，第三方兼容网关（openai-next）走 Bearer。
	// 两个都带是刻意为之：官方会忽略 Authorization，网关两种都接受。
	// 这条断言改成"必须带 x-api-key，Bearer 可有可无"，避免把实测过的兼容行为误判为错误。
	if got.Header.Get("Authorization") != "" && !strings.HasPrefix(got.Header.Get("Authorization"), "Bearer ") {
		t.Errorf("Authorization 头格式异常: %q", got.Header.Get("Authorization"))
	}
	if got.Body["system"] != "sys" {
		t.Errorf("system 必须为顶层字段: %v", got.Body)
	}
	msgs, _ := got.Body["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("system 不应混入 messages: %v", msgs)
	}
	oc, _ := got.Body["output_config"].(map[string]any)
	f, _ := oc["format"].(map[string]any)
	if f["type"] != "json_schema" {
		t.Errorf("结构化输出字段错误: %v", got.Body["output_config"])
	}
	if got.Body["max_tokens"] == nil {
		t.Errorf("max_tokens 必填")
	}
}

func TestClaudeErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"unknown field output_config"}}`))
	}))
	defer srv.Close()
	t.Setenv("TEST_CL_KEY", "k")
	cfg := testCfg(srv.URL)
	cfg.Claude.MaxRetries = 0
	c := NewClient(cfg)
	res := c.Call(t.Context(), ProviderClaude, &Request{Messages: []Message{{Role: "user", Content: "u"}}})
	if res.Kind != ErrBadRequest {
		t.Fatalf("400 应判为 bad_request 并降级，得到 %v", res.Kind)
	}
	if res.Err == nil || !containsSub(res.Err.Error(), "output_config") {
		t.Errorf("应保留服务端错误说明以便排查: %v", res.Err)
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
