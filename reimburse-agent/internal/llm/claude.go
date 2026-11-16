package llm

import (
	"encoding/json"
	"strings"
)

// claude 适配：Anthropic Messages API（/v1/messages）。
// system 是顶层字段，不在 messages 里；结构化输出用 output_config.format（GA，无需 beta 头）。
// 该字段在部分代理或旧版兼容层上不存在，解析失败时上层会报 bad_request 并降级，不会静默放行。
type claude struct {
	ep  *Endpoint
	key string
}

func newClaude(ep *Endpoint, key string) *claude { return &claude{ep: ep, key: key} }

func (c *claude) Provider() string { return ProviderClaude }
func (c *claude) Model() string    { return c.ep.Model }

func (c *claude) endpoint() string {
	base := strings.TrimRight(c.ep.BaseURL, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/messages"
	}
	return base + "/v1/messages"
}

type clMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type clReq struct {
	Model        string    `json:"model"`
	System       string    `json:"system,omitempty"`
	Messages     []clMsg   `json:"messages"`
	MaxTokens    int       `json:"max_tokens"`
	Temperature  float64   `json:"temperature"`
	OutputConfig *clOutCfg `json:"output_config,omitempty"`
	Stream       bool      `json:"stream"`
}

type clOutCfg struct {
	Format *clFormat `json:"format,omitempty"`
}

type clFormat struct {
	Type   string         `json:"type"`
	Schema map[string]any `json:"schema"`
}

func (c *claude) build(req *Request) ([]byte, map[string]string, error) {
	msgs := make([]clMsg, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, clMsg{Role: m.Role, Content: m.Content})
	}
	body := clReq{Model: c.ep.Model, System: req.System, Messages: msgs,
		MaxTokens: pickInt(req.MaxTokens, c.ep.MaxTokens), Temperature: req.Temperature, Stream: false}
	if c.ep.JSONMode && req.Schema != nil {
		body.OutputConfig = &clOutCfg{Format: &clFormat{Type: "json_schema", Schema: req.Schema}}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	// 官方 Anthropic 只认 x-api-key；部分第三方兼容网关（如 openai-next）走 Bearer。
	// 两个都带：官方网关会忽略 Authorization，兼容网关通常也接受 x-api-key，
	// 手上这两个头同时存在不会导致鉴权失败（已对两种网关实测）。
	return b, map[string]string{
		"x-api-key":         c.key,
		"Authorization":     "Bearer " + c.key,
		"anthropic-version": "2023-06-01",
		"Accept":            "application/json",
	}, nil
}

func (c *claude) parse(status int, body []byte) (string, Usage, *Error) {
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string          `json:"stop_reason"`
		Usage      json.RawMessage `json:"usage"`
		Error      *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", Usage{}, &Error{Kind: ErrBadJSON, Status: status, Message: "响应不是合法 JSON: " + snippet(body)}
	}
	if r.Error != nil && r.Error.Message != "" {
		return "", Usage{}, &Error{Kind: ErrServer, Status: status, Message: r.Error.Type + ": " + r.Error.Message}
	}
	var u Usage
	decodeUsage(r.Usage, &u.InputTokens, &u.OutputTokens, &u.CacheHitIn)
	var sb strings.Builder
	for _, b := range r.Content {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	text := sb.String()
	switch r.StopReason {
	case "max_tokens":
		return text, u, &Error{Kind: ErrEmpty, Status: status, Message: "输出被 max_tokens 截断"}
	case "refusal":
		return text, u, &Error{Kind: ErrBadJSON, Status: status, Message: "模型拒绝作答"}
	}
	if strings.TrimSpace(text) == "" {
		return text, u, &Error{Kind: ErrEmpty, Status: status, Message: "没有文本内容"}
	}
	return text, u, nil
}
