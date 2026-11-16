// Package llm 是 DeepSeek 与 Claude 的统一调用层。
//
// 设计约束（与项目整体一致）：
//   - 只做字段抽取和文字生成，判定结论由规则引擎给出，模型不参与判定；
//   - 密钥只从环境变量读取，不允许写进配置文件，日志和界面里只出现脱敏后的名称；
//   - 任何模型调用失败、超时、返回不合法 JSON，都必须降级为"无法核验"，不能当作通过；
//   - 同一个 Prompt 在两家模型上都用 JSON 结构化输出模式，避免靠正则从散文里抠字段。
package llm

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// 供应商名称，用于日志、界面和交叉比对。
const (
	ProviderDeepSeek = "deepseek"
	ProviderClaude   = "claude"
	ProviderOffline  = "offline" // 离线回放，仅作备注用；本包不实现回放
)

// Role 是交叉比对中一个模型的角色。
type Role string

const (
	RolePrimary  Role = "primary"  // 主模型：先跑，成功即可用时接受
	RoleVerifier Role = "verifier" // 校验模型：对关键字段独立再抽一遍，与主模型比对
)

// Endpoint 描述一个供应商的接入方式。
type Endpoint struct {
	Provider    string        `json:"provider"`
	BaseURL     string        `json:"base_url"`
	Model       string        `json:"model"`
	APIKeyEnv   string        `json:"api_key_env"`        // 只存环境变量名，不存密钥本身
	Timeout     time.Duration `json:"-"`                  // 单次请求超时
	TimeoutSec  float64       `json:"timeout_sec"`        // 配置文件里的秒数
	MaxRetries  int           `json:"max_retries"`        // 超时、5xx、429 和空响应的重试次数
	MaxTokens   int           `json:"max_tokens"`         // 输出上限，防止 JSON 被截断
	Temperature float64       `json:"temperature"`        // 抽取类任务用 0
	JSONMode    bool          `json:"json_mode"`          // 是否启用供应商的结构化输出
	PriceIn     float64       `json:"price_in_per_mtok"`  // 美元/百万输入 token，取自官方价格页，仅供成本估算
	PriceOut    float64       `json:"price_out_per_mtok"` // 美元/百万输出 token
	PriceNote   string        `json:"price_note,omitempty"`
	// UserAgent 部分第三方网关会拦非浏览器 UA（返回 Cloudflare 1010），需要显式设置。
	UserAgent string `json:"user_agent,omitempty"`
}

// Config 是两家模型的接入配置。DeepSeek 与 Claude 各一个。
type Config struct {
	DeepSeek Endpoint `json:"deepseek"`
	Claude   Endpoint `json:"claude"`
}

// DefaultConfig 返回默认配置。
//
// 模型名与价格核实日期：2026-11-16。
//   - DeepSeek 官方文档：模型名 deepseek-flash（源码模型 DeepSeek-V4.1-Flash-1017），
//     OpenAI 兼容地址 https://api.deepseek.com，支持 response_format json_object 与工具调用。
//     价格为峰时 $0.30/$1.20 每百万 token（缓存未命中输入/输出），谷时为其一半。
//     这里按峰时计，宁可高估成本。
//   - Claude：Anthropic Messages API，https://api.anthropic.com，当前可用模型 Claude Sonnet 5.5，
//     $2/$10 每百万 token。结构化输出走 output_config.format（GA），不需要 beta 头。
//
// 两个模型名都放在配置文件里，现场换模型改配置即可，不用改代码。
func DefaultConfig() Config {
	return Config{
		DeepSeek: Endpoint{
			Provider: ProviderDeepSeek, BaseURL: "https://api.deepseek.com", Model: "deepseek-flash",
			APIKeyEnv: "DEEPSEEK_API_KEY", TimeoutSec: 90, MaxRetries: 1, MaxTokens: 4096,
			Temperature: 0, JSONMode: true, PriceIn: 0.30, PriceOut: 1.20,
			PriceNote: "峰时价，谷时减半；按缓存未命中计",
		},
		Claude: Endpoint{
			Provider: ProviderClaude, BaseURL: "https://api.anthropic.com", Model: "claude-sonnet-5-5",
			APIKeyEnv: "ANTHROPIC_API_KEY", TimeoutSec: 120, MaxRetries: 1, MaxTokens: 4096,
			// 第三方兼容网关会拦非浏览器 UA（Cloudflare 1010）。
			UserAgent:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
			Temperature: 0, JSONMode: true, PriceIn: 2, PriceOut: 10,
			PriceNote: "Sonnet 5.5 列表价",
		},
	}
}

// LoadConfig 读取配置文件；path 为空时返回默认配置。
// 配置文件中不允许出现任何密钥字段，出现即报错，避免把密钥写进仓库。
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		return cfg, cfg.Normalize()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	// 按结构体逐字段检查有没有"看起来像密钥"的键。
	// 不能用整段文本转小写后找 "api_key\""：字段名 api_key_env 本身就以 api_key 开头，
	// 会被误判成"配置文件里写了密钥"而拒绝启动（曾经真的踩到）。
	// 这里只认完全等于这些名字的键，并递归进对象。
	if bad, where := findSecretKey(b); bad != "" {
		return cfg, fmt.Errorf("配置文件 %s 在 %s 处出现密钥字段 %q，密钥只能放在环境变量中", path, where, bad)
	}
	dec := json.NewDecoder(strings.NewReader(string(stripComments(b))))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("解析模型配置: %w", err)
	}
	return cfg, cfg.Normalize()
}

// Normalize 补齐缺省值并校验。
func (c *Config) Normalize() error {
	for _, ep := range []*Endpoint{&c.DeepSeek, &c.Claude} {
		if ep.BaseURL == "" || ep.Model == "" || ep.APIKeyEnv == "" {
			return fmt.Errorf("%s 配置缺少 base_url、model 或 api_key_env", ep.Provider)
		}
		if ep.TimeoutSec <= 0 {
			ep.TimeoutSec = 60
		}
		ep.Timeout = time.Duration(ep.TimeoutSec * float64(time.Second))
		if ep.MaxRetries < 0 {
			ep.MaxRetries = 0
		}
		if ep.MaxTokens <= 0 {
			ep.MaxTokens = 2048
		}
	}
	return nil
}

// Endpoint 按供应商名取配置。
func (c *Config) Endpoint(provider string) (*Endpoint, bool) {
	switch provider {
	case ProviderDeepSeek:
		return &c.DeepSeek, true
	case ProviderClaude:
		return &c.Claude, true
	}
	return nil, false
}

// APIKey 从环境变量取密钥。不落盘、不回显；为空表示该供应商未配置。
func (e *Endpoint) APIKey() string {
	return strings.TrimSpace(os.Getenv(e.APIKeyEnv))
}

// Redacted 返回可以安全写进日志和界面的描述，例如 "deepseek/deepseek-flash（密钥 DEEPSEEK_API_KEY 已配置）"。
func (e *Endpoint) Redacted() string {
	state := "未配置"
	if e.APIKey() != "" {
		state = "已配置"
	}
	return fmt.Sprintf("%s/%s（密钥 %s %s）", e.Provider, e.Model, e.APIKeyEnv, state)
}

// CostUSD 按官方列表价估算一次调用的成本（美元），用于界面上的成本统计。
func (e *Endpoint) CostUSD(in, out int) float64 {
	return float64(in)/1e6*e.PriceIn + float64(out)/1e6*e.PriceOut
}

// findSecretKey 递归查找形如被禁止的键名。返回命中的键名与所在路径。
func findSecretKey(b []byte) (string, string) {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return "", ""
	}
	banned := map[string]bool{
		"api_key": true, "apikey": true, "key": true, "secret": true,
		"token": true, "access_token": true, "password": true,
	}
	var walk func(v any, path string) (string, string)
	walk = func(v any, path string) (string, string) {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				if banned[strings.ToLower(k)] {
					return k, path
				}
				if name, where := walk(child, path+"."+k); name != "" {
					return name, where
				}
			}
		case []any:
			for i, child := range t {
				if name, where := walk(child, fmt.Sprintf("%s[%d]", path, i)); name != "" {
					return name, where
				}
			}
		}
		return "", ""
	}
	return walk(v, "（根）")
}

// stripComments 去掉以下划线开头的键（用于在 JSON 里写说明）。
// 只处理对象成员，不碰字符串内容。
func stripComments(b []byte) []byte {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return b
	}
	var prune func(any) any
	prune = func(x any) any {
		switch t := x.(type) {
		case map[string]any:
			out := map[string]any{}
			for k, child := range t {
				if strings.HasPrefix(k, "_") {
					continue
				}
				out[k] = prune(child)
			}
			return out
		case []any:
			out := make([]any, len(t))
			for i, child := range t {
				out[i] = prune(child)
			}
			return out
		}
		return x
	}
	cleaned, err := json.Marshal(prune(v))
	if err != nil {
		return b
	}
	return cleaned
}
