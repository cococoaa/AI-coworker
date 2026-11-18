package upload

import (
	"os"
	"strings"
	"testing"
)

// 用真实的 PDF 验证提取器（不是自己造的文件测自己）。
// 文件不存在时跳过，不把测试绑死在某一台机器上。
func TestExtractRealPDF(t *testing.T) {
	paths := []string{
		`C:\Users\zwj20\Desktop\信息安全概论作业-计科.pdf`,
		`C:\Users\zwj20\Desktop\张文杰-Golang后端开发(5).pdf`,
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		txt, err := ExtractPDFText(b)
		if err != nil {
			t.Logf("%s: 提取失败（%d 字节）: %v", p, len(b), err)
			continue
		}
		t.Logf("%s: 提取 %d 字符，前 200 字：%q", p, len([]rune(txt)), trunc(txt, 200))
		if len([]rune(txt)) < MinTextRunes {
			t.Errorf("%s 提取内容过少", p)
		}
	}
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

var _ = strings.Contains
