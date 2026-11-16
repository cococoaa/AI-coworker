package llm

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"reimburse/internal/schema"
)

// 合成抽取只用于"无密钥演示"，但必须走与真实输出完全相同的解析与比对路径，
// 否则演示出来的"降级效果"是假的。

func loadOne(t *testing.T) *schema.Case {
	t.Helper()
	c, err := schema.LoadCase("../../data/cases/C01")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSyntheticPassesSchema(t *testing.T) {
	c := loadOne(t)
	for _, p := range []string{ProviderDeepSeek, ProviderClaude} {
		ex, issue := (&Synthetic{}).SyntheticExtract(context.Background(), p, c)
		if issue != nil {
			t.Fatalf("%s 合成输出未通过 Schema 校验: %v", p, issue)
		}
		if ex.Fields[FInvoiceNo].Value != c.Truth.Invoice.InvoiceNo {
			t.Errorf("%s 发票号不一致", p)
		}
		if ex.Fields[FTotalAmount].Value != "553.00" {
			t.Errorf("%s 金额格式不对: %q", p, ex.Fields[FTotalAmount].Value)
		}
	}
}

// 污损字段必须表现为"读不出来 + 低置信度"，这样才会走 R15-b 的转人工路径。
func TestSyntheticUnreadable(t *testing.T) {
	c, err := schema.LoadCase("../../data/cases/C59")
	if err != nil {
		t.Fatal(err)
	}
	ex, issue := (&Synthetic{}).SyntheticExtract(context.Background(), ProviderDeepSeek, c)
	if issue != nil {
		t.Fatal(issue)
	}
	if v := ex.Fields[FIssueDate]; v.Value != "" || v.Conf >= LowThreshold {
		t.Errorf("污损字段应读不出且置信度低，实际 %+v", v)
	}
}

// 确定性：同一条用例、同一供应商，两次合成结果必须一致，演示才可复现。
func TestSyntheticDeterministic(t *testing.T) {
	c := loadOne(t)
	a := (&Synthetic{}).Build(c, ProviderClaude)
	b := (&Synthetic{}).Build(c, ProviderClaude)
	if a.Text != b.Text {
		t.Fatal("合成结果不确定，演示无法复现")
	}
}

// 合成打标：录制文件里必须写明"合成，非真实模型输出"，不能被当成真实录制。
func TestSyntheticTapeIsLabeled(t *testing.T) {
	c := loadOne(t)
	dir := t.TempDir()
	if err := WriteSyntheticTape(dir, ProviderDeepSeek, c); err != nil {
		t.Fatal(err)
	}
	rp := &Replayer{Dir: dir}
	tape, err := rp.Load(ProviderDeepSeek, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tape.RecordedAt == "" || tape.Model == "" {
		t.Fatal("录制文件缺少来源标注")
	}
	if tape.Model == "deepseek-flash" {
		t.Fatal("合成数据不能伪装成真实模型")
	}
}

// 合成输出必须严格等于标准答案：两个供应商字段完全一致、置信度 1.0。
// 这里锁死"不要在合成里加噪声"——加噪声会凭空制造两模型不一致，
// 让评测里的命中项对不上（曾经 15 条用例误报 R15-b）。降级路径由彩排用开关演示。
func TestSyntheticEqualsTruth(t *testing.T) {
	dir := "../../data/cases"
	entries, err := osReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		c, err := schema.LoadCase(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Truth.Unreadable) > 0 {
			continue // 污损字段本来就该读不出来
		}
		a, _ := (&Synthetic{}).SyntheticExtract(context.Background(), ProviderDeepSeek, c)
		b, _ := (&Synthetic{}).SyntheticExtract(context.Background(), ProviderClaude, c)
		if a == nil || b == nil {
			t.Fatalf("%s 合成失败", c.ID)
		}
		d := &DualResult{A: a, B: b}
		d.Compare()
		if len(d.Disagree) != 0 {
			t.Errorf("%s 两个供应商合成结果不一致: %v", c.ID, d.Disagree)
		}
		if len(d.LowConf) != 0 {
			t.Errorf("%s 合成结果不应触发低置信度: %v", c.ID, d.LowConf)
		}
		checked++
	}
	if checked < 40 {
		t.Fatalf("检查用例太少: %d", checked)
	}
}

func osReadDir(p string) ([]os.DirEntry, error) { return os.ReadDir(p) }
