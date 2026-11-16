package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Message 是一条对话消息。System 单独放，不混进 Messages（两家 API 都是这么分的）。
type Message struct {
	Role    string `json:"role"` // user / assistant
	Content string `json:"content"`
}

// Request 是一次模型调用。
type Request struct {
	System      string
	Messages    []Message
	Schema      map[string]any // JSON Schema；为空时不启用结构化输出
	SchemaName  string
	MaxTokens   int
	Temperature float64
}

// Usage 是 token 用量，用于成本估算和界面展示。
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	CacheHitIn   int `json:"cache_hit_input_tokens,omitempty"`
}

// Response 是模型返回。Raw 保留原始文本，便于审计与回放。
type Response struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Text      string `json:"text"`
	Usage     Usage  `json:"usage"`
	LatencyMS int64  `json:"latency_ms"`
	Attempts  int    `json:"attempts"`
}

// Call 是带错误分类的调用结果，供上层记录"未核验"。
type Call struct {
	Resp *Response
	Err  error
	Kind ErrKind
}

// ErrKind 是错误分类。上层据此决定重试、切换模型还是直接转人工。
type ErrKind string

const (
	ErrNone        ErrKind = ""
	ErrNoKey       ErrKind = "no_key"      // 未配置密钥
	ErrAuth        ErrKind = "auth"        // 401/403，密钥无效
	ErrRateLimit   ErrKind = "rate_limit"  // 429
	ErrTimeout     ErrKind = "timeout"     // 超时或客户端主动取消
	ErrServer      ErrKind = "server"      // 5xx
	ErrBadRequest  ErrKind = "bad_request" // 其它 4xx，通常是 Prompt 或 Schema 问题
	ErrEmpty       ErrKind = "empty"       // 空响应或被截断
	ErrBadJSON     ErrKind = "bad_json"    // 返回内容不是合法 JSON 或不符合 Schema
	ErrNetwork     ErrKind = "network"     // 连接失败、DNS 等
	ErrUnsupported ErrKind = "unsupported" // 供应商/配置不支持
	ErrDisabled    ErrKind = "disabled"    // 演示开关关闭，模拟模型不可用
)

// Error 是带分类的错误。
type Error struct {
	Kind    ErrKind
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s (HTTP %d): %s", e.Kind, e.Status, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

// retryable 表示这类错误值得重试一次：超时、限流、服务端错误、网络抖动。
func (e *Error) retryable() bool {
	switch e.Kind {
	case ErrTimeout, ErrRateLimit, ErrServer, ErrNetwork, ErrEmpty:
		return true
	}
	return false
}

// friendly 把错误翻成给审核员看的一句话，界面直接显示。
func (e *Error) friendly(provider string) string {
	name := providerName(provider)
	switch e.Kind {
	case ErrNoKey:
		return name + "未配置密钥，无法调用"
	case ErrDisabled:
		return name + "已被关闭（演示开关），未参与本次审核"
	case ErrTimeout:
		return name + "调用超时，状态未知"
	case ErrAuth:
		return name + "密钥无效或无权限"
	case ErrRateLimit:
		return name + "被限流"
	case ErrServer:
		return name + "服务端错误"
	case ErrEmpty:
		return name + "返回为空或被截断"
	case ErrBadJSON:
		return name + "返回内容不是合法 JSON"
	case ErrNetwork:
		return name + "网络不可达"
	}
	return name + "调用失败"
}

func providerName(p string) string {
	switch p {
	case ProviderDeepSeek:
		return "DeepSeek"
	case ProviderClaude:
		return "Claude"
	}
	return p
}

// ExtractJSON 从模型输出中取出 JSON 对象。
// 结构化输出模式下应当直接是 JSON；这里仍容忍 ```json 代码块和前后多余文字，
// 但取不到合法 JSON 就返回 ErrBadJSON，绝不猜字段。
func ExtractJSON(s string) ([]byte, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil, &Error{Kind: ErrBadJSON, Message: "返回内容为空"}
	}
	if i := strings.Index(t, "```"); i >= 0 {
		rest := t[i+3:]
		if j := strings.Index(rest, "```"); j >= 0 {
			rest = rest[:j]
			rest = strings.TrimPrefix(rest, "json")
			t = strings.TrimSpace(rest)
		}
	}
	if !json.Valid([]byte(t)) {
		start, end := strings.Index(t, "{"), strings.LastIndex(t, "}")
		if start < 0 || end <= start || !json.Valid([]byte(t[start:end+1])) {
			return nil, &Error{Kind: ErrBadJSON, Message: "找不到合法 JSON 对象"}
		}
		t = t[start : end+1]
	}
	return []byte(t), nil
}
