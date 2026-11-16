// Package mcpauth 是 MCP 核验服务的令牌鉴权。
//
// 设计要点：
//   - 令牌只从环境变量或权限受限的密钥文件读取，绝不写进命令行参数（避免出现在进程列表和 shell 历史里）。
//   - 服务端用常量时间比较，且把"令牌个数"也纳入比较，避免按响应时间猜出前缀。
//   - 每个令牌有名字，日志里只记名字与指纹前 8 位，不记令牌本身。
//   - 客户端一律用 Bearer 头；本包提供 RoundTripper，保证重连、SSE 长连接等请求都带上令牌。
//   - 没有配置令牌时，服务拒绝在非本机地址上监听 —— 默认安全，不允许"忘了配置就裸奔"。
package mcpauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 服务端环境变量：逗号分隔的多个令牌，格式 "名字:令牌"；只写令牌时名字记为 token-N。
const EnvTokens = "REIMBURSE_MCP_TOKENS"

// 客户端环境变量：单个令牌。
const EnvClientToken = "REIMBURSE_MCP_TOKEN"

// 界面访问的令牌环境变量（与 MCP 服务用同一套校验逻辑，但允许单独发令牌）。
const EnvUITokens = "REIMBURSE_UI_TOKENS"

// TokenTTL 是本服务的令牌有效期约定。静态令牌本身不带过期时间，
// 这里给一个上限，让中间件与审计日志都有明确的时间语义。
const TokenTTL = 24 * time.Hour

// Entry 是一个已登记的令牌（只在内存里保存哈希）。
type Entry struct {
	Name string
	Hash [32]byte
}

// Verifier 实现 auth.TokenVerifier。
type Verifier struct {
	entries []Entry
	// lastUsed 记录每个令牌最近一次使用时间，便于发现"令牌泄露后仍在被用"。
	mu       sync.Mutex
	lastUsed map[string]time.Time
	now      func() time.Time
}

var ErrNoToken = errors.New("未配置任何令牌（" + EnvTokens + " 为空）")

// NewVerifier 从配置的令牌列表构造校验器。
// 每个令牌至少 16 个字符，太短的一律拒绝启动，避免现场随手设个 "123"。
func NewVerifier(tokens []string) (*Verifier, error) {
	if len(tokens) == 0 {
		return nil, ErrNoToken
	}
	v := &Verifier{lastUsed: map[string]time.Time{}, now: time.Now}
	seen := map[[32]byte]bool{}
	for i, t := range tokens {
		name, secret := splitToken(t, i)
		if len(secret) < 16 {
			return nil, fmt.Errorf("令牌 %q 长度不足 16 位，请换一个更长的随机串", name)
		}
		h := sha256.Sum256([]byte(secret))
		if seen[h] {
			return nil, fmt.Errorf("令牌 %q 重复", name)
		}
		seen[h] = true
		v.entries = append(v.entries, Entry{Name: name, Hash: h})
	}
	sort.Slice(v.entries, func(i, j int) bool { return v.entries[i].Name < v.entries[j].Name })
	return v, nil
}

// splitToken 解析 "名字:令牌"。
func splitToken(s string, i int) (name, secret string) {
	s = strings.TrimSpace(s)
	if k := strings.Index(s, ":"); k > 0 && k < len(s)-1 && !strings.Contains(s[:k], " ") {
		return s[:k], s[k+1:]
	}
	return fmt.Sprintf("token-%d", i+1), s
}

// Verify 实现 auth.TokenVerifier。
//
// 比较方式：对每个条目做一次常量时间比较，并且全部比较完再汇总结果，
// 不提前返回，避免通过响应时间逐个猜出入口。
func (v *Verifier) Verify(ctx context.Context, token string, req *http.Request) (*auth.TokenInfo, error) {
	h := sha256.Sum256([]byte(token))
	found := -1
	for i, e := range v.entries {
		if subtle.ConstantTimeCompare(e.Hash[:], h[:]) == 1 && found < 0 {
			found = i
		}
	}
	if found < 0 {
		return nil, fmt.Errorf("%w: 指纹 %s", auth.ErrInvalidToken, Fingerprint(token))
	}
	e := v.entries[found]
	v.mu.Lock()
	v.lastUsed[e.Name] = v.now()
	v.mu.Unlock()
	return &auth.TokenInfo{
		Expiration: v.now().Add(TokenTTL),
		UserID:     e.Name,
		Scopes:     []string{"mcp:read"},
		Extra:      map[string]any{"client": e.Name, "ip": clientIP(req)},
	}, nil
}

// Names 返回已登记的令牌名（不含令牌本身），用于启动日志。
func (v *Verifier) Names() []string {
	var out []string
	for _, e := range v.entries {
		out = append(out, e.Name)
	}
	return out
}

// LastUsed 返回每个令牌最近使用时间，供运维排查。
func (v *Verifier) LastUsed() map[string]time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := map[string]time.Time{}
	for k, t := range v.lastUsed {
		out[k] = t
	}
	return out
}

// Fingerprint 返回令牌的指纹（sha256 前 8 位十六进制），可以安全写进日志。
func Fingerprint(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:4])
}

// LoadTokens 读取令牌：配置了环境变量就用环境变量，否则读密钥文件。
// 两者都配置时以环境变量为准，不会悄悄把两边的令牌合并（避免"以为只剩一个令牌"）。
// 密钥文件每行一个令牌（"名字:令牌" 或仅令牌），空行与 # 开头的行忽略。
func LoadTokens(env string, file string) ([]string, string, error) {
	source := ""
	var raw []string
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		raw = strings.Split(v, ",")
		source = "环境变量 " + env
		file = "" // 环境变量优先，不再读文件
	}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, "", fmt.Errorf("读取令牌文件: %w", err)
		}
		if err := CheckFilePerm(file); err != nil {
			return nil, "", err
		}
		for _, ln := range strings.Split(string(b), "\n") {
			ln = strings.TrimSpace(ln)
			if ln == "" || strings.HasPrefix(ln, "#") {
				continue
			}
			raw = append(raw, ln)
		}
		source = "文件 " + file
	}
	var out []string
	for _, t := range raw {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out, source, nil
}

// CheckFilePerm 在类 Unix 系统上要求密钥文件不对同组和其他用户可读。
// Windows 的 Go 运行时把权限位固定映射成 0666，无法表达 ACL，
// 因此在 Windows 上跳过检查（文档要求把令牌文件放在用户目录下）。
func CheckFilePerm(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("令牌文件 %s 权限过宽（%v），请执行 chmod 600 %s", path, st.Mode().Perm(), path)
	}
	return nil
}

// clientIP 取客户端地址，只保留 IP，便于在日志里追踪来源。
func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if h := r.Header.Get("X-Forwarded-For"); h != "" {
		if i := strings.Index(h, ","); i > 0 {
			h = h[:i]
		}
		return strings.TrimSpace(h)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- 客户端 ---

// TokenSource 提供当前令牌。每次请求都重新取，便于运行期轮换。
type TokenSource func() string

// StaticToken 返回固定令牌。
func StaticToken(t string) TokenSource { return func() string { return t } }

// Transport 给每个请求加 Bearer 头。用它包住 http.Client，
// MCP 客户端在重连、建立 SSE 长连接时也会自动带上令牌。
type Transport struct {
	Base  http.RoundTripper
	Token TokenSource
}

func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	tok := t.Token()
	if tok == "" {
		return nil, errors.New("未配置访问令牌")
	}
	// 克隆请求，避免修改调用方的 request。
	rc := r.Clone(r.Context())
	rc.Header.Set("Authorization", "Bearer "+tok)
	return base.RoundTrip(rc)
}

// HTTPClient 返回带令牌的客户端。
func HTTPClient(tok TokenSource, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &Transport{Base: http.DefaultTransport, Token: tok},
		Timeout:   timeout, // 0 表示由调用方 context 控制
	}
}

// ClientTransport 构造 Streamable HTTP 传输，已带令牌。
func ClientTransport(endpoint string, tok TokenSource, timeout time.Duration) *mcp.StreamableClientTransport {
	return &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: HTTPClient(tok, timeout),
	}
}

// LoadClientToken 读取客户端令牌：环境变量优先，其次文件。返回令牌本身（不打印）。
func LoadClientToken(env, file string) (string, string, error) {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v, "环境变量 " + env, nil
	}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", "", fmt.Errorf("读取令牌文件: %w", err)
		}
		if err := CheckFilePerm(file); err != nil {
			return "", "", err
		}
		for _, ln := range strings.Split(string(b), "\n") {
			ln = strings.TrimSpace(ln)
			if ln == "" || strings.HasPrefix(ln, "#") {
				continue
			}
			name, secret := splitToken(ln, 0)
			_ = name
			return secret, "文件 " + file, nil
		}
		return "", "", errors.New("令牌文件为空")
	}
	return "", "", errors.New("未配置客户端令牌（" + env + " 或 -token-file）")
}
