package mcpauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const tokA = "test-token-abcdefgh-0001"
const tokB = "test-token-abcdefgh-0002"

func mustVerifier(t *testing.T, toks ...string) *Verifier {
	t.Helper()
	v, err := NewVerifier(toks)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// 未带令牌、令牌错误、令牌正确三种情况。
func TestMiddleware(t *testing.T) {
	v := mustVerifier(t, "reviewer:"+tokA)
	h := v.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ti := tokenFromContext(r)
		w.WriteHeader(200)
		if ti != nil {
			_, _ = w.Write([]byte(ti.UserID))
		}
	}))

	cases := []struct {
		name, header string
		want         int
	}{
		{"缺少令牌", "", 401},
		{"格式错误", "Token " + tokA, 401},
		{"令牌错误", "Bearer wrong-token-00000000", 401},
		{"令牌正确", "Bearer " + tokA, 200},
		{"大小写不敏感的前缀", "bearer " + tokA, 200},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/mcp", nil)
		if c.header != "" {
			req.Header.Set("Authorization", c.header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s：期望 %d，得到 %d (%s)", c.name, c.want, rec.Code, rec.Body.String())
		}
	}
}

// tokenFromContext 取当前请求的令牌信息，仅供测试读取 UserID。
func tokenFromContext(r *http.Request) *TokenInfo { return TokenInfoFrom(r.Context()) }

// TokenInfo 是本包对 SDK 令牌信息的别名，避免测试里到处写 auth.xxx。
type TokenInfo = sdkTokenInfo

type sdkTokenInfo = auth.TokenInfo

// 鉴权必须把令牌信息写回同一个请求对象，否则外层的审计中间件读不到调用方。
func TestContextWrittenToSameRequest(t *testing.T) {
	v := mustVerifier(t, "reviewer:"+tokA)
	var seen string
	h := v.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ti := TokenInfoFrom(r.Context()); ti != nil {
			seen = ti.UserID
		}
	}))
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+tokA)
	// 模拟审计中间件：先挂 sink，再交给内层鉴权。
	inner, s := WithSink(req)
	h.ServeHTTP(httptest.NewRecorder(), inner)
	if seen != "reviewer" {
		t.Fatalf("下游没读到令牌信息: %q", seen)
	}
	if s.Client() != "reviewer" {
		t.Fatalf("外层读不到调用方，审计日志会缺 client: %q", s.Client())
	}
}

// 401 响应必须带 WWW-Authenticate，客户端才知道该用 Bearer。
func TestUnauthorizedChallenge(t *testing.T) {
	v := mustVerifier(t, tokA)
	h := v.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/mcp", nil))
	if rec.Code != 401 {
		t.Fatalf("应 401，得到 %d", rec.Code)
	}
	if !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("缺少 Bearer 挑战头: %q", rec.Header().Get("WWW-Authenticate"))
	}
}

// 错误响应里不能出现令牌本身，只能出现指纹。
func TestNoTokenLeakInResponse(t *testing.T) {
	v := mustVerifier(t, tokA)
	h := v.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	bad := tokA[:len(tokA)-1] + "X"
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+bad)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, bad) || strings.Contains(body, tokA) {
		t.Fatalf("响应里泄漏了令牌: %s", body)
	}
	if !strings.Contains(body, Fingerprint(bad)) {
		t.Errorf("应给出指纹便于排查: %s", body)
	}
}

// 多个令牌各自独立生效，且能识别调用方。
func TestMultipleTokens(t *testing.T) {
	v := mustVerifier(t, "reviewer:"+tokA, "auditor:"+tokB)
	var seen []string
	h := v.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ti := tokenFromContext(r)
		seen = append(seen, ti.UserID)
	}))
	for _, tk := range []string{tokA, tokB} {
		req := httptest.NewRequest("POST", "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+tk)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if len(seen) != 2 || seen[0] != "auditor" || seen[1] != "reviewer" && seen[1] != "auditor" {
		// 名字按字母序登记，这里只校验两个名字都被正确识别。
		set := map[string]bool{}
		for _, s := range seen {
			set[s] = true
		}
		if !set["reviewer"] || !set["auditor"] {
			t.Fatalf("调用方识别错误: %v", seen)
		}
	}
}

// 太短的令牌拒绝启动：现场随手设 "123" 等于没有鉴权。
func TestRejectShortToken(t *testing.T) {
	if _, err := NewVerifier([]string{"123456"}); err == nil {
		t.Fatal("短令牌应被拒绝")
	}
	if _, err := NewVerifier(nil); err == nil {
		t.Fatal("空令牌列表应被拒绝")
	}
}

func TestRejectDuplicateToken(t *testing.T) {
	if _, err := NewVerifier([]string{"a:" + tokA, "b:" + tokA}); err == nil {
		t.Fatal("重复令牌应被拒绝")
	}
}

// 令牌名与令牌本体分离；没有名字时自动编号。
func TestTokenNaming(t *testing.T) {
	v := mustVerifier(t, tokA)
	if v.Names()[0] != "token-1" {
		t.Errorf("未命名的令牌应记为 token-1: %v", v.Names())
	}
}

// 令牌文件：支持注释与空行，权限过宽时拒绝。
func TestLoadTokens(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tokens")
	content := "# 注释行\n\nreviewer:" + tokA + "\n" + tokB + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	toks, src, err := LoadTokens("NO_SUCH_ENV_XYZ", p)
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 2 || src == "" {
		t.Fatalf("解析结果错误: %v %q", toks, src)
	}
	if _, err := NewVerifier(toks); err != nil {
		t.Fatalf("从文件加载的令牌应可用: %v", err)
	}
	// 环境变量优先。
	t.Setenv("REIMBURSE_TEST_TOKENS", "env:"+tokB)
	toks, _, err = LoadTokens("REIMBURSE_TEST_TOKENS", p)
	if err != nil || len(toks) != 1 || toks[0] != "env:"+tokB {
		t.Fatalf("环境变量应优先: %v %v", toks, err)
	}
}

// 客户端 Transport 必须给每个请求带上 Bearer 头（含重连与 SSE 长连接）。
func TestClientTransportSetsHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := HTTPClient(StaticToken(tokA), 5*time.Second)
	req, _ := http.NewRequest("GET", srv.URL, nil)
	if _, err := c.Do(req); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer "+tokA {
		t.Fatalf("请求头错误: %q", got)
	}
	// 原 request 不被修改。
	if req.Header.Get("Authorization") != "" {
		t.Error("不应修改调用方的 request")
	}
}

func TestClientTransportWithoutToken(t *testing.T) {
	c := HTTPClient(StaticToken(""), 5*time.Second)
	req, _ := http.NewRequest("GET", "http://127.0.0.1:1/", nil)
	if _, err := c.Do(req); err == nil {
		t.Fatal("没有令牌时应直接报错，而不是发一个无令牌请求")
	}
}

// 端到端：带鉴权的 MCP 服务，无令牌被拒、有令牌成功。
func TestMCPWithAuthEndToEnd(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "s", Version: "1"}, nil)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	v := mustVerifier(t, tokA)
	hs := httptest.NewServer(v.Middleware()(handler))
	defer hs.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 无令牌：连接失败。
	if _, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).
		Connect(ctx, ClientTransport(hs.URL, StaticToken(""), 5*time.Second), nil); err == nil {
		t.Fatal("无令牌不应连上")
	}
	// 错误令牌：连接失败。
	if _, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).
		Connect(ctx, ClientTransport(hs.URL, StaticToken(tokB), 5*time.Second), nil); err == nil {
		t.Fatal("错误令牌不应连上")
	}
	// 正确令牌：连上并完成初始化。
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).
		Connect(ctx, ClientTransport(hs.URL, StaticToken(tokA), 5*time.Second), nil)
	if err != nil {
		t.Fatalf("正确令牌应连上: %v", err)
	}
	defer sess.Close()
	if _, err := sess.ListTools(ctx, &mcp.ListToolsParams{}); err != nil {
		t.Fatalf("鉴权后应能列出工具: %v", err)
	}
}

// 审计需要的字段：令牌只用名字与指纹标识。
func TestFingerprintStableAndShort(t *testing.T) {
	a := Fingerprint(tokA)
	if a != Fingerprint(tokA) {
		t.Fatal("指纹应稳定")
	}
	if len(a) != 8 || strings.Contains(a, tokA) {
		t.Fatalf("指纹格式错误: %q", a)
	}
	if a == Fingerprint(tokB) {
		t.Fatal("不同令牌指纹应不同")
	}
	b, _ := json.Marshal(map[string]string{"fp": a})
	if strings.Contains(string(b), tokA) {
		t.Fatal("序列化后不应含令牌")
	}
}
