package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 这里用本地假服务器，不起外网请求：验证的是本包的超时、重试、错误分类与解析逻辑。

func testCfg(base string) *Config {
	cfg := DefaultConfig()
	cfg.DeepSeek.BaseURL = base
	cfg.Claude.BaseURL = base
	cfg.DeepSeek.APIKeyEnv = "TEST_DS_KEY"
	cfg.Claude.APIKeyEnv = "TEST_CL_KEY"
	for _, ep := range []*Endpoint{&cfg.DeepSeek, &cfg.Claude} {
		ep.TimeoutSec = 1
		ep.MaxRetries = 1
		ep.MaxTokens = 512
	}
	_ = cfg.Normalize()
	return &cfg
}

func dsBody(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 120, "completion_tokens": 30},
	})
	return string(b)
}

func TestNoKey(t *testing.T) {
	t.Setenv("TEST_DS_KEY", "")
	c := NewClient(testCfg("http://127.0.0.1:1"))
	res := c.Call(context.Background(), ProviderDeepSeek, &Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if res.Kind != ErrNoKey {
		t.Fatalf("未配置密钥应返回 no_key，得到 %v", res.Kind)
	}
}

func TestRetryOnServerErrorThenSucceed(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":{"message":"boom"}}`)
			return
		}
		fmt.Fprint(w, dsBody(`{"ok":1}`))
	}))
	defer srv.Close()
	t.Setenv("TEST_DS_KEY", "k")
	c := NewClient(testCfg(srv.URL))
	res := c.Call(context.Background(), ProviderDeepSeek, &Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if res.Err != nil {
		t.Fatalf("重试后应成功: %v", res.Err)
	}
	if res.Resp.Attempts != 2 {
		t.Errorf("应为第 2 次尝试成功，得到 %d", res.Resp.Attempts)
	}
	if res.Resp.Usage.InputTokens != 120 || res.Resp.Usage.OutputTokens != 30 {
		t.Errorf("用量解析错误: %+v", res.Resp.Usage)
	}
}

func TestTimeoutIsClassifiedAndNotRetriedForever(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		time.Sleep(1500 * time.Millisecond)
		fmt.Fprint(w, dsBody(`{}`))
	}))
	defer srv.Close()
	t.Setenv("TEST_DS_KEY", "k")
	cfg := testCfg(srv.URL)
	cfg.DeepSeek.MaxRetries = 1
	c := NewClient(cfg)
	start := time.Now()
	res := c.Call(context.Background(), ProviderDeepSeek, &Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if res.Kind != ErrTimeout {
		t.Fatalf("应判定为超时，得到 %v (%v)", res.Kind, res.Err)
	}
	if got := atomic.LoadInt32(&n); got != 2 {
		t.Errorf("超时应重试一次，共 2 次请求，实际 %d", got)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("超时控制失效，耗时 %v", el)
	}
	friendly := res.Err.(*Error).friendly(ProviderDeepSeek)
	if !strings.Contains(friendly, "状态未知") {
		t.Errorf("超时的提示应说明状态未知: %s", friendly)
	}
}

func TestAuthErrorNotRetried(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"message":"invalid key"}}`)
	}))
	defer srv.Close()
	t.Setenv("TEST_DS_KEY", "k")
	c := NewClient(testCfg(srv.URL))
	res := c.Call(context.Background(), ProviderDeepSeek, &Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if res.Kind != ErrAuth {
		t.Fatalf("401 应判为 auth，得到 %v", res.Kind)
	}
	if n != 1 {
		t.Errorf("鉴权失败不应重试，实际请求 %d 次", n)
	}
}

func TestRateLimitRetried(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"message":"slow down"}}`)
			return
		}
		fmt.Fprint(w, `{"content":[{"type":"text","text":"{\"a\":1}"}],"stop_reason":"end_turn","usage":{}}`)
	}))
	defer srv.Close()
	t.Setenv("TEST_CL_KEY", "k")
	c := NewClient(testCfg(srv.URL))
	res := c.Call(context.Background(), ProviderClaude, &Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if res.Err != nil || res.Resp.Attempts != 2 {
		t.Fatalf("429 后应重试成功: %v attempts=%v", res.Err, res.Resp)
	}
}

func TestEmptyContentIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, dsBody("   "))
	}))
	defer srv.Close()
	t.Setenv("TEST_DS_KEY", "k")
	cfg := testCfg(srv.URL)
	cfg.DeepSeek.MaxRetries = 0
	c := NewClient(cfg)
	res := c.Call(context.Background(), ProviderDeepSeek, &Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if res.Kind != ErrEmpty {
		t.Fatalf("空内容应判为 empty（不能当作成功），得到 %v", res.Kind)
	}
}

func TestClaudeParsesContentBlocks(t *testing.T) {
	var gotSystem string
	var gotOutput map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotSystem, _ = body["system"].(string)
		gotOutput, _ = body["output_config"].(map[string]any)
		if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"content":[{"type":"text","text":"{\"a\":1}"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	defer srv.Close()
	t.Setenv("TEST_CL_KEY", "k")
	c := NewClient(testCfg(srv.URL))
	res := c.Call(context.Background(), ProviderClaude, &Request{
		System: "sys", Messages: []Message{{Role: "user", Content: "x"}},
		Schema: map[string]any{"type": "object"}, SchemaName: "t",
	})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.Resp.Text != `{"a":1}` {
		t.Errorf("文本拼接错误: %q", res.Resp.Text)
	}
	if gotSystem != "sys" {
		t.Errorf("system 应为顶层字段，实际 %q", gotSystem)
	}
	if _, ok := gotOutput["format"]; !ok {
		t.Errorf("启用结构化输出时应带 output_config.format，实际 %v", gotOutput)
	}
	if res.Resp.Usage.OutputTokens != 5 {
		t.Errorf("用量解析错误: %+v", res.Resp.Usage)
	}
}

func TestClaudeMaxTokensIsTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"content":[{"type":"text","text":"{\"a\":"}],"stop_reason":"max_tokens","usage":{}}`)
	}))
	defer srv.Close()
	t.Setenv("TEST_CL_KEY", "k")
	cfg := testCfg(srv.URL)
	cfg.Claude.MaxRetries = 0
	c := NewClient(cfg)
	res := c.Call(context.Background(), ProviderClaude, &Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if res.Kind != ErrEmpty || !strings.Contains(res.Err.Error(), "截断") {
		t.Fatalf("被截断的响应必须报错，得到 %v %v", res.Kind, res.Err)
	}
}

func TestExtractJSONTolerance(t *testing.T) {
	cases := map[string]bool{
		`{"a":1}`:                 true,
		"```json\n{\"a\":1}\n```": true,
		`结果如下：{"a":1} 以上`:         true,
		`这不是 JSON`:                false,
		``:                        false,
		`{"a":1} 和 {"b":2}`:       false,
	}
	for in, ok := range cases {
		_, err := ExtractJSON(in)
		if (err == nil) != ok {
			t.Errorf("%q 期望 ok=%v，得到 err=%v", in, ok, err)
		}
	}
}

func TestParseExtractionRejectsBadShape(t *testing.T) {
	ok := `{"fields":{"invoice_no":{"value":"1","confidence":1},"issue_date":{"value":"2026-10-01","confidence":1},"total_amount":{"value":"553.00","confidence":1},"buyer_tax_id":{"value":"X","confidence":1},"seller_tax_id":{"value":"Y","confidence":1},"buyer_name":{"value":"A","confidence":1},"seller_name":{"value":"B","confidence":1},"item_name":{"value":"C","confidence":1}},"notes":""}`
	ex, issue := parseExtraction(ok)
	if issue != nil || ex == nil {
		t.Fatalf("合法输出被拒绝: %v", issue)
	}
	if ex.Fields[FTotalAmount].Value != "553.00" {
		t.Errorf("字段值错误: %+v", ex.Fields[FTotalAmount])
	}
	// 置信度越界必须拒绝，不能静默截断。
	bad := strings.Replace(ok, `"confidence":1`, `"confidence":1.5`, 1)
	if _, issue := parseExtraction(bad); issue == nil {
		t.Error("置信度越界应被拒绝")
	}
	// 多出未知字段必须拒绝（DisallowUnknownFields），防止模型自造字段。
	unknown := `{"fields":{},"notes":"","extra":1}`
	if _, issue := parseExtraction(unknown); issue == nil {
		t.Error("未知字段应被拒绝")
	}
}

func TestNormalizeValue(t *testing.T) {
	if got := normalizeValue(FTotalAmount, " 553 "); got != "553.00" {
		t.Errorf("金额归一化: %q", got)
	}
	if got := normalizeValue(FTotalAmount, "553.5"); got != "553.50" {
		t.Errorf("金额归一化: %q", got)
	}
	if got := normalizeValue(FSellerTaxID, " 91310103692b6r5g35 "); got != "91310103692B6R5G35" {
		t.Errorf("税号归一化: %q", got)
	}
}

func TestDualExtractCompare(t *testing.T) {
	mk := func(total string) *Extraction {
		return &Extraction{Fields: map[string]FieldValue{
			FInvoiceNo: {Value: "1", Conf: 1}, FIssueDate: {Value: "2026-10-01", Conf: 1},
			FTotalAmount: {Value: total, Conf: 0.95}, FBuyerTaxID: {Value: "B", Conf: 1},
			FSellerTaxID: {Value: "S", Conf: 0.9}, FBuyerName: {Value: "N", Conf: 1},
			FSellerName: {Value: "M", Conf: 1}, FItemName: {Value: "I", Conf: 1},
		}}
	}
	d := &DualResult{A: mk("553.00"), B: mk("553")}
	d.compare()
	if len(d.Disagree) != 0 {
		t.Errorf("金额 553.00 与 553 应视为一致，误差字段: %v", d.Disagree)
	}
	if d.Fields[FTotalAmount].Conf != 0.95 {
		t.Errorf("合并置信度应取两侧较小者，得到 %v", d.Fields[FTotalAmount].Conf)
	}

	d = &DualResult{A: mk("553.00"), B: mk("553.01")}
	d.compare()
	if len(d.Disagree) != 1 || d.Disagree[0] != FTotalAmount {
		t.Errorf("金额不一致应被识别，得到 %v", d.Disagree)
	}
	if !containsStr(d.Untrusted, FTotalAmount) {
		t.Errorf("不一致字段应标为不可信: %v", d.Untrusted)
	}
	if d.Fields[FTotalAmount].Value != "" {
		t.Errorf("不一致时不应任选一边的值，得到 %q", d.Fields[FTotalAmount].Value)
	}

	// 只有一边成功：降级但可用，界面需显示降级说明。
	d = &DualResult{A: mk("553.00"), B: nil}
	d.compare()
	if d.BothOK || d.Degraded == "" {
		t.Errorf("单边成功应标记降级: %+v", d)
	}
	if d.Fields[FTotalAmount].Value != "553.00" {
		t.Errorf("单边成功时沿用可用的一边: %+v", d.Fields[FTotalAmount])
	}

	// 低置信度要单独列出，供 R15-b 使用。
	e := mk("553.00")
	e.Fields[FSellerTaxID] = FieldValue{Value: "S", Conf: 0.5}
	d = &DualResult{A: e, B: mk("553.00")}
	d.compare()
	if !containsStr(d.LowConf, FSellerTaxID) {
		t.Errorf("低置信度字段应被列出: %v", d.LowConf)
	}
}

func TestConfigRejectsInlineKey(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/cfg.json"
	if err := os.WriteFile(p, []byte(`{"deepseek":{"api_key":"sk-xxx"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("配置文件里出现 api_key 字段必须报错")
	}
}

func TestRedactedHidesKey(t *testing.T) {
	t.Setenv("TEST_DS_KEY", "sk-very-secret")
	ep := Endpoint{Provider: ProviderDeepSeek, Model: "m", APIKeyEnv: "TEST_DS_KEY"}
	s := ep.Redacted()
	if strings.Contains(s, "very-secret") {
		t.Fatalf("脱敏描述里不应出现密钥: %s", s)
	}
	if !strings.Contains(s, "已配置") {
		t.Errorf("应说明密钥已配置: %s", s)
	}
}

func containsStr(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}
