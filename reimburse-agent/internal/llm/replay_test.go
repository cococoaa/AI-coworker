package llm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTapeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	rec := &Recorder{Dir: dir}
	resp := &Response{Provider: ProviderDeepSeek, Model: "m", Text: `{"fields":{}}`,
		Usage: Usage{InputTokens: 10, OutputTokens: 2}, LatencyMS: 33, Attempts: 1}
	if err := rec.Save(ProviderDeepSeek, "C01", resp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ProviderDeepSeek, "C01.json")); err != nil {
		t.Fatalf("录制文件未写入: %v", err)
	}
	rp := &Replayer{Dir: dir}
	tape, err := rp.Load(ProviderDeepSeek, "C01")
	if err != nil {
		t.Fatal(err)
	}
	if tape.Text != resp.Text || tape.Usage.InputTokens != 10 {
		t.Errorf("回放内容不一致: %+v", tape)
	}
	if tape.RecordedAt == "" {
		t.Errorf("录制文件应标注来源")
	}
	listed := rp.Listed()
	if len(listed[ProviderDeepSeek]) != 1 || listed[ProviderDeepSeek][0] != "C01" {
		t.Errorf("清单错误: %v", listed)
	}
}

// 录制文件缺失时必须报错，不能返回空字段让流程继续。
func TestReplayMissingFails(t *testing.T) {
	rp := &Replayer{Dir: t.TempDir()}
	if _, issue := rp.ReplayExtract(context.Background(), ProviderClaude, "C99", nil); issue == nil {
		t.Fatal("没有录制时应返回错误")
	}
}

// Prompt 版本变了，旧录制必须失效。
func TestReplayPromptVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ProviderClaude, "C01.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"provider":"claude","prompt_version":0,"text":"{}"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rp := &Replayer{Dir: dir}
	_, err := rp.Load(ProviderClaude, "C01")
	if err == nil {
		t.Fatal("版本不一致应拒绝")
	}
}

// 回放得到的字段仍要过 Schema 校验，坏数据不能混进判定。
func TestReplayInvalidJSONRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ProviderDeepSeek, "C01.json")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(`{"provider":"deepseek","prompt_version":1,"text":"不是 json"}`), 0o644)
	rp := &Replayer{Dir: dir}
	_, issue := rp.ReplayExtract(context.Background(), ProviderDeepSeek, "C01", nil)
	if issue == nil || issue.Kind != ErrBadJSON {
		t.Fatalf("坏录制应判为 bad_json，得到 %v", issue)
	}
}

var _ = errors.Is
