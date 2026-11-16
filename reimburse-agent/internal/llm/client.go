package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// provider 是一个供应商的协议适配。统一入口只依赖这个接口，
// 换模型、换成 DeepSeek 的 Anthropic 兼容地址都只影响对应实现。
type provider interface {
	Provider() string
	Model() string
	// build 构造请求体。
	build(req *Request) (body []byte, headers map[string]string, err error)
	// endpoint 返回完整 URL。
	endpoint() string
	// parse 解析响应体，返回文本和用量。
	parse(status int, body []byte) (text string, usage Usage, err *Error)
}

// Client 是统一调用入口：超时、重试、错误分类、空响应处理都在这里，
// 各供应商只负责协议转换。
type Client struct {
	HTTP  *http.Client
	Cfg   *Config
	Now   func() time.Time // 便于测试固定时间
	seedR *rand.Rand       // 重试退避的抖动，测试时可固定
}

// NewClient 创建统一调用入口。timeout 取各供应商配置中较大者作为整体上限。
func NewClient(cfg *Config) *Client {
	return &Client{
		HTTP:  &http.Client{Timeout: 0}, // 单次超时由 context 控制，便于精确分类
		Cfg:   cfg,
		Now:   time.Now,
		seedR: rand.New(rand.NewSource(1)),
	}
}

// Call 按供应商调用模型。
// 返回的 Call.Err 为 nil 表示拿到了合法响应文本（还不代表 JSON 合法，由调用方按 Schema 解析）。
func (c *Client) Call(ctx context.Context, providerName string, req *Request) Call {
	ep, ok := c.Cfg.Endpoint(providerName)
	if !ok {
		return Call{Err: &Error{Kind: ErrUnsupported, Message: providerName}, Kind: ErrUnsupported}
	}
	key := ep.APIKey()
	if key == "" {
		return Call{Err: &Error{Kind: ErrNoKey, Message: ep.APIKeyEnv + " 为空"}, Kind: ErrNoKey}
	}
	p, err := newProvider(ep, key)
	if err != nil {
		return Call{Err: err, Kind: ErrUnsupported}
	}
	return c.call(ctx, p, ep, req)
}

// call 是重试循环。只对可重试错误重试，最多 ep.MaxRetries 次。
func (c *Client) call(ctx context.Context, p provider, ep *Endpoint, req *Request) Call {
	start := c.Now()
	attempts := 0
	var lastErr *Error
	for attempts <= ep.MaxRetries {
		attempts++
		text, usage, herr := c.once(ctx, p, ep, req)
		if herr == nil {
			if strings.TrimSpace(text) == "" {
				// 结构化输出模式下偶发返回纯空白（已知问题），当作空响应重试。
				lastErr = &Error{Kind: ErrEmpty, Message: "响应文本为空"}
			} else {
				return Call{Kind: ErrNone, Resp: &Response{
					Provider: p.Provider(), Model: p.Model(), Text: text, Usage: usage,
					LatencyMS: c.Now().Sub(start).Milliseconds(), Attempts: attempts,
				}}
			}
		} else {
			lastErr = herr
		}
		if !lastErr.retryable() || attempts > ep.MaxRetries {
			break
		}
		// 退避：限流 1s 起，其余 200ms 起，加 0-100ms 抖动。
		base := 200 * time.Millisecond
		if lastErr.Kind == ErrRateLimit {
			base = time.Second
		}
		d := base * time.Duration(1<<(attempts-1))
		d += time.Duration(c.seedR.Intn(100)) * time.Millisecond
		select {
		case <-ctx.Done():
			lastErr = &Error{Kind: ErrTimeout, Message: "调用被取消"}
			return Call{Err: lastErr, Kind: ErrTimeout}
		case <-time.After(d):
		}
	}
	lastErr.Message = strings.TrimSpace(lastErr.Message)
	if lastErr.Message == "" {
		lastErr.Message = "重试耗尽"
	}
	return Call{Err: lastErr, Kind: lastErr.Kind}
}

// once 发一次请求。
func (c *Client) once(ctx context.Context, p provider, ep *Endpoint, req *Request) (string, Usage, *Error) {
	body, headers, err := p.build(req)
	if err != nil {
		return "", Usage{}, &Error{Kind: ErrBadRequest, Message: err.Error()}
	}
	rctx, cancel := context.WithTimeout(ctx, ep.Timeout)
	defer cancel()
	hr, err := http.NewRequestWithContext(rctx, http.MethodPost, p.endpoint(), bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, &Error{Kind: ErrBadRequest, Message: err.Error()}
	}
	hr.Header.Set("Content-Type", "application/json")
	// 部分第三方网关会拦非浏览器 UA（返回 Cloudflare error 1010），
	// 因此允许在配置里指定 UA，默认为官方 SDK 习惯的标识。
	ua := ep.UserAgent
	if ua == "" {
		ua = "reimburse-audit/1.0"
	}
	hr.Header.Set("User-Agent", ua)
	for k, v := range headers {
		hr.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(hr)
	if err != nil {
		var ne interface{ Timeout() bool }
		switch {
		case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
			return "", Usage{}, &Error{Kind: ErrTimeout, Message: err.Error()}
		case errors.Is(err, context.Canceled):
			return "", Usage{}, &Error{Kind: ErrTimeout, Message: "调用被取消"}
		}
		return "", Usage{}, &Error{Kind: ErrNetwork, Message: err.Error()}
	}
	defer resp.Body.Close()
	// 限制读取大小，防止异常响应把内存吃满。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", Usage{}, &Error{Kind: ErrNetwork, Message: err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return "", Usage{}, classifyStatus(resp.StatusCode, raw)
	}
	text, usage, perr := p.parse(resp.StatusCode, raw)
	if perr != nil {
		return "", usage, perr
	}
	return text, usage, nil
}

// classifyStatus 把 HTTP 状态码归类，并尽量带上服务端错误说明。
func classifyStatus(status int, body []byte) *Error {
	msg := snippet(body)
	switch {
	case status == 401 || status == 403:
		return &Error{Kind: ErrAuth, Status: status, Message: msg}
	case status == 429:
		return &Error{Kind: ErrRateLimit, Status: status, Message: msg}
	case status >= 500:
		return &Error{Kind: ErrServer, Status: status, Message: msg}
	}
	return &Error{Kind: ErrBadRequest, Status: status, Message: msg}
}

// snippet 截取错误响应里的有用部分，最多 300 字节。
func snippet(b []byte) string {
	b = bytes.TrimSpace(b)
	if len(b) > 300 {
		b = b[:300]
	}
	s := strings.TrimSpace(string(b))
	return strings.ReplaceAll(s, "\n", " ")
}

// newProvider 按供应商名创建适配。
func newProvider(ep *Endpoint, key string) (provider, error) {
	switch ep.Provider {
	case ProviderDeepSeek:
		return newDeepSeek(ep, key), nil
	case ProviderClaude:
		return newClaude(ep, key), nil
	}
	return nil, &Error{Kind: ErrUnsupported, Message: ep.Provider}
}

// decodeUsage 容错解析 usage，缺失字段按 0 处理。
func decodeUsage(raw json.RawMessage, in, out, hit *int) {
	if len(raw) == 0 {
		return
	}
	// 不能直接反序列化成 map[string]json.Number：usage 里出现字符串字段
	// （例如 service_tier:"standard"、output_tokens_details 这种嵌套对象）时，
	// 整个 unmarshal 会失败，用量就全变成 0。第三方网关的 usage 字段尤其不统一。
	// 这里先解析成任意值，再逐个按键取数字，遇到非数字就跳过该键。
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	get := func(keys ...string) int {
		for _, k := range keys {
			switch v := m[k].(type) {
			case float64:
				return int(v)
			case json.Number:
				if n, err := v.Int64(); err == nil {
					return int(n)
				}
			}
		}
		return 0
	}
	*in = get("prompt_tokens", "input_tokens")
	*out = get("completion_tokens", "output_tokens")
	*hit = get("prompt_cache_hit_tokens", "cache_read_input_tokens")
}
