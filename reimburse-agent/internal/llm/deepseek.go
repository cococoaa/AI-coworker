package llm

import (
	"encoding/json"
	"strings"
)

// deepSeek 适配：OpenAI 兼容的 /chat/completions。
// 结构化输出用 response_format={"type":"json_object"}，
// 官方要求 Prompt 里必须出现 "json" 字样并给出格式示例，否则输出不稳定。
type deepSeek struct {
	ep  *Endpoint
	key string
}

func newDeepSeek(ep *Endpoint, key string) *deepSeek { return &deepSeek{ep: ep, key: key} }

func (d *deepSeek) Provider() string { return ProviderDeepSeek }
func (d *deepSeek) Model() string    { return d.ep.Model }

func (d *deepSeek) endpoint() string {
	base := strings.TrimRight(d.ep.BaseURL, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

type dsMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type dsReq struct {
	Model          string         `json:"model"`
	Messages       []dsMsg        `json:"messages"`
	MaxTokens      int            `json:"max_tokens,omitempty"`
	Temperature    float64        `json:"temperature"`
	ResponseFormat *dsResponseFmt `json:"response_format,omitempty"`
	Stream         bool           `json:"stream"`
}

type dsResponseFmt struct {
	Type string `json:"type"`
}

func (d *deepSeek) build(req *Request) ([]byte, map[string]string, error) {
	msgs := make([]dsMsg, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, dsMsg{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, dsMsg{Role: m.Role, Content: m.Content})
	}
	body := dsReq{Model: d.ep.Model, Messages: msgs, MaxTokens: pickInt(req.MaxTokens, d.ep.MaxTokens),
		Temperature: pickTemp(req.Temperature), Stream: false}
	if d.ep.JSONMode && req.Schema != nil {
		body.ResponseFormat = &dsResponseFmt{Type: "json_object"}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	return b, map[string]string{"Authorization": "Bearer " + d.key}, nil
}

func (d *deepSeek) parse(status int, body []byte) (string, Usage, *Error) {
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", Usage{}, &Error{Kind: ErrBadJSON, Status: status, Message: "响应不是合法 JSON: " + snippet(body)}
	}
	if r.Error != nil && r.Error.Message != "" {
		return "", Usage{}, &Error{Kind: ErrServer, Status: status, Message: r.Error.Message}
	}
	var u Usage
	decodeUsage(r.Usage, &u.InputTokens, &u.OutputTokens, &u.CacheHitIn)
	if len(r.Choices) == 0 {
		return "", u, &Error{Kind: ErrEmpty, Status: status, Message: "没有 choices"}
	}
	ch := r.Choices[0]
	if ch.FinishReason == "length" {
		return ch.Message.Content, u, &Error{Kind: ErrEmpty, Status: status, Message: "输出被 max_tokens 截断"}
	}
	return ch.Message.Content, u, nil
}

func pickInt(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func pickTemp(v float64) float64 { return v }
