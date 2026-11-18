package upload

import (
	"os"
	"strings"
	"testing"
)

// 真实电子发票的典型形态：Identity-H 编码 + UTF-16BE 十六进制字符串。
//
// 这是最需要验证的一种：内容流里的"字"是十六进制编码，
// 不查 CMap、不按 UTF-16 解，读出来就是乱码。
// 生成脚本见测试数据说明；这里直接读文件，不存在时跳过。
func TestExtractInvoiceStylePDF(t *testing.T) {
	b, err := os.ReadFile("../../data/samples/invoice-identity-h.pdf")
	if err != nil {
		t.Skip("缺少发票样例 PDF")
	}
	txt, err := ExtractPDFText(b)
	if err != nil {
		t.Fatalf("应能提取: %v", err)
	}
	for _, want := range []string{
		"电子发票", "26157293145980000777", "2026年11月10日",
		"星河科技有限公司", "91310115MA1FL8X3K0", "73.00",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("缺少 %q\n提取结果:\n%s", want, txt)
		}
	}
	if !looksLikeInvoice(txt) {
		t.Error("应识别出这是发票")
	}
}

// 整条链路：PDF 上传 → 提取 → 落盘 → 得到可审单据。
func TestSavePDFInvoice(t *testing.T) {
	b, err := os.ReadFile("../../data/samples/invoice-identity-h.pdf")
	if err != nil {
		t.Skip("缺少发票样例 PDF")
	}
	p := &Processor{Root: t.TempDir()}
	res, err := p.Save(Request{
		Claim:       sampleClaim(),
		InvoiceName: "电子发票.pdf",
		InvoiceData: b,
	})
	if err != nil {
		t.Fatalf("PDF 应能保存: %v", err)
	}
	if res.Case.HasTruth {
		t.Error("上传票据不应有标准答案")
	}
	if !strings.Contains(res.Case.InvoiceText, "26157293145980000777") {
		t.Errorf("落盘的发票文本应含号码，实际:\n%s", res.Case.InvoiceText)
	}
}
