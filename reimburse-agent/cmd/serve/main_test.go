package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"reimburse/internal/mcpauth"
)

// 界面登录页的行为：未带凭证的浏览器请求给登录框，API 请求仍回 401，
// 令牌正确时种 HttpOnly Cookie，脚本用 Bearer 也照常可用。

const testToken = "ui-token-abcdefgh-0001"

func handlerFor(t *testing.T) (http.Handler, *mcpauth.Verifier) {
	t.Helper()
	v, err := mcpauth.NewVerifier([]string{"guest:" + testToken})
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	return uiAuth(v, next, false), v
}

func TestBrowserGetsLoginPage(t *testing.T) {
	h, _ := handlerFor(t)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("未带凭证的浏览器应被引到登录页，得到 %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("跳转地址错误: %q", loc)
	}
}

func TestLoginPageHasForm(t *testing.T) {
	h, _ := handlerFor(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	if rec.Code != 200 {
		t.Fatalf("登录页状态码 %d", rec.Code)
	}
	body := rec.Body.String()
	for _, need := range []string{`name="token"`, "/login", "访问令牌"} {
		if !strings.Contains(body, need) {
			t.Errorf("登录页缺少 %q", need)
		}
	}
}

func TestAPIStillReturns401(t *testing.T) {
	h, _ := handlerFor(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/meta", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("API 无令牌应 401，得到 %d", rec.Code)
	}
	// 不能把 HTML 登录页返回给脚本。
	if strings.Contains(rec.Body.String(), "<form") {
		t.Error("API 请求不应返回登录页")
	}
}

func TestLoginRejectsWrongToken(t *testing.T) {
	h, _ := handlerFor(t)
	form := url.Values{"token": {"wrong-token-xxxxxxxx"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "令牌不正确") {
		t.Fatalf("错误令牌应停在登录页并提示，得到 %d", rec.Code)
	}
	if strings.Contains(rec.Header().Get("Set-Cookie"), testToken) {
		t.Error("错误令牌不应种 Cookie")
	}
}

func TestLoginAcceptsTokenAndSetsCookie(t *testing.T) {
	h, _ := handlerFor(t)
	form := url.Values{"token": {testToken}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("正确令牌应跳转到首页，得到 %d：%s", rec.Code, rec.Body.String())
	}
	c := rec.Header().Get("Set-Cookie")
	if !strings.Contains(c, "HttpOnly") || !strings.Contains(c, "SameSite=Strict") {
		t.Errorf("Cookie 必须是 HttpOnly + SameSite=Strict: %q", c)
	}
}

func TestCookieGrantsAccess(t *testing.T) {
	h, _ := handlerFor(t)
	req := httptest.NewRequest("GET", "/api/meta", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: testToken})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("带合法 Cookie 应放行，得到 %d", rec.Code)
	}
}

func TestBearerStillWorks(t *testing.T) {
	h, _ := handlerFor(t)
	req := httptest.NewRequest("GET", "/api/meta", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("Bearer 应放行，得到 %d", rec.Code)
	}
}

func TestModelScopeProxyUsesAppCredentials(t *testing.T) {
	_, verifier := handlerFor(t)
	h := uiAuth(verifier, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), true)
	for _, tc := range []struct {
		name, cookie, header string
		want                 int
	}{
		{name: "proxy credential only", want: http.StatusUnauthorized},
		{name: "application cookie", cookie: testToken, want: http.StatusOK},
		{name: "application header", header: testToken, want: http.StatusOK},
		{name: "wrong cookie", cookie: "incorrect-token", want: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/meta", nil)
			req.Header.Set("Authorization", "Bearer modelscope-proxy-token")
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: cookieName, Value: tc.cookie})
			}
			if tc.header != "" {
				req.Header.Set("X-Reimburse-Token", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("got HTTP %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestWrongCookieRejected(t *testing.T) {
	h, _ := handlerFor(t)
	req := httptest.NewRequest("GET", "/api/meta", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "nope-nope-nope"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误 Cookie 应 401，得到 %d", rec.Code)
	}
}
