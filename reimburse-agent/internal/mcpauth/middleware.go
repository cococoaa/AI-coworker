package mcpauth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// tokenInfoKey 是本包写入 context 的键。
type tokenInfoKey struct{}

// sinkKey 用于审计：鉴权中间件把调用方名字写进 sink，审计中间件在外层读取。
// HTTP 中间件无法把内层新建的 context 回传给外层，所以用这一个小对象做单向传递。
type sinkKey struct{}

// sink 是一个只写一次的字符串槽。
type sink struct{ v string }

// SetClient 供鉴权中间件写入调用方，供测试与审计使用。
func (s *sink) SetClient(name string) { s.v = name }

// Client 返回调用方名。
func (s *sink) Client() string { return s.v }

// WithSink 在请求上挂一个 sink，返回的指针由调用方读取结果。
func WithSink(r *http.Request) (*http.Request, *sink) {
	s := &sink{}
	return r.WithContext(context.WithValue(r.Context(), sinkKey{}, s)), s
}

// TokenInfoFrom 读取令牌信息，语义与 SDK 的 auth.TokenInfoFromContext 相同。
func TokenInfoFrom(ctx context.Context) *auth.TokenInfo {
	ti, _ := ctx.Value(tokenInfoKey{}).(*auth.TokenInfo)
	return ti
}

// Middleware 返回鉴权中间件。
//
// 只做一层校验，不叠加 SDK 的 RequireBearerToken（两层会对同一请求重复读取）。
// 一次校验同时完成：
//  1. 通过后把令牌信息写进请求 context，下游可用 TokenInfoFrom 读取；
//  2. 若请求上挂了 sink（WithSink），同时写入调用方名字，供外层审计中间件使用；
//  3. 失败统一返回 401 并带 WWW-Authenticate 挑战头，错误信息只含令牌指纹。
//
// 用法：auditLogMiddleware( v.Middleware()( handler ) )，审计放外层。
func (v *Verifier) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ti, msg, code := v.authorize(r)
			if code != 0 {
				w.Header().Set("WWW-Authenticate", `Bearer scope="mcp:read"`)
				http.Error(w, msg, code)
				return
			}
			if s, ok := r.Context().Value(sinkKey{}).(*sink); ok {
				s.SetClient(ti.UserID)
			}
			// 只替换 context 指针，不拷贝整个请求：拷贝会连带改写 body 的包装状态。
			r = r.WithContext(context.WithValue(r.Context(), tokenInfoKey{}, ti))
			next.ServeHTTP(w, r)
		})
	}
}

// authorize 校验请求里的 Bearer 令牌。
func (v *Verifier) authorize(r *http.Request) (*auth.TokenInfo, string, int) {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "bearer") {
		return nil, "no bearer token", http.StatusUnauthorized
	}
	ti, err := v.Verify(r.Context(), fields[1], r)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidToken) {
			return nil, err.Error(), http.StatusUnauthorized
		}
		return nil, "token validation failed", http.StatusInternalServerError
	}
	if ti == nil {
		return nil, "token validation failed", http.StatusInternalServerError
	}
	if ti.Expiration.IsZero() {
		return nil, "token missing expiration", http.StatusUnauthorized
	}
	if ti.Expiration.Before(time.Now()) {
		return nil, "token expired", http.StatusUnauthorized
	}
	return ti, "", 0
}
