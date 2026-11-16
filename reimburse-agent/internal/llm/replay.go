package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 离线回放：把真实模型跑过的响应存成录制文件，在没有密钥时回放，
// 用于现场演示兜底和回归测试。
//
// 约束（写在文档和界面上）：
//   - 回放数据一律标注"录制回放"，绝不冒充实时调用；
//   - 录制文件按"供应商 + 单据 + prompt 版本"寻址，找不到就报错，不猜；
//   - 回放不产生任何成本，界面显示的成本应标注为"估算"。
//
// 只有在 Extract 之前已有录制时才用回放；正常有密钥时始终走真实调用。

// Recorder 记录一次抽取的真实响应，用于日后回放。
type Recorder struct {
	Dir string
}

// Tape 是一份录制内容。
type Tape struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	CaseID     string `json:"case_id"`
	PromptV    int    `json:"prompt_version"`
	Text       string `json:"text"`
	Usage      Usage  `json:"usage"`
	LatencyMS  int64  `json:"latency_ms"`
	RecordedAt string `json:"recorded_at"`
}

// tapeVersion 是抽取 Prompt 的版本号。Prompt 一改，旧录制自动失效，
// 避免用旧 Prompt 的输出冒充新 Prompt 的结果。
const tapeVersion = 1

// TapePath 返回录制文件路径。
func TapePath(dir, provider, caseID string) string {
	return filepath.Join(dir, provider, caseID+".json")
}

// Save 写入录制文件。
func (r *Recorder) Save(provider, caseID string, resp *Response) error {
	p := TapePath(r.Dir, provider, caseID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	t := Tape{Provider: provider, Model: resp.Model, CaseID: caseID, PromptV: tapeVersion,
		Text: resp.Text, Usage: resp.Usage, LatencyMS: resp.LatencyMS,
		RecordedAt: "录制于真实调用"}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// Replayer 在没有密钥时提供录制响应。
type Replayer struct {
	Dir string
}

// Load 读取录制。版本不匹配视为不存在。
func (r *Replayer) Load(provider, caseID string) (*Tape, error) {
	b, err := os.ReadFile(TapePath(r.Dir, provider, caseID))
	if err != nil {
		return nil, err
	}
	var t Tape
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("录制文件损坏: %w", err)
	}
	if t.PromptV != tapeVersion {
		return nil, fmt.Errorf("录制版本 %d 与当前 Prompt 版本 %d 不一致，请重新录制", t.PromptV, tapeVersion)
	}
	if strings.TrimSpace(t.Text) == "" {
		return nil, fmt.Errorf("录制内容为空")
	}
	return &t, nil
}

// Listed 返回已录制的用例编号（按供应商分组），用于界面提示哪些单据可回放。
func (r *Replayer) Listed() map[string][]string {
	out := map[string][]string{}
	for _, prov := range []string{ProviderDeepSeek, ProviderClaude} {
		entries, err := os.ReadDir(filepath.Join(r.Dir, prov))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			out[prov] = append(out[prov], strings.TrimSuffix(e.Name(), ".json"))
		}
		sort.Strings(out[prov])
	}
	return out
}

// ReplayExtract 实现 LLM 接口，但只读录制。
func (r *Replayer) ReplayExtract(ctx context.Context, provider, caseID string, onCall func(*Response)) (*Extraction, *SchemaIssue) {
	t, err := r.Load(provider, caseID)
	if err != nil {
		return nil, &SchemaIssue{Kind: ErrUnsupported, Detail: "无可用的录制：" + err.Error()}
	}
	resp := &Response{Provider: provider, Model: t.Model, Text: t.Text, Usage: t.Usage,
		LatencyMS: t.LatencyMS, Attempts: 1}
	if onCall != nil {
		onCall(resp)
	}
	ex, issue := parseExtraction(resp.Text)
	if issue != nil {
		return nil, issue
	}
	ex.Provider, ex.Model = provider, t.Model
	ex.Usage, ex.LatencyMS, ex.Attempts = t.Usage, t.LatencyMS, 1
	return ex, nil
}

var _ = (*Recorder)(nil)
