package upload

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"strings"
	"testing"

	"reimburse/internal/schema"
)

// makePDF 造一个最小的文本型 PDF，内容用 Tj 操作符写出。
//
// 刻意不引第三方 PDF 库来"造测试文件再测自己"——那样两边可能犯同一个错。
// 这里手写 PDF 结构，规则来自 PDF 1.4 规范里最基础的部分。
func makePDF(lines []string, compress bool) []byte {
	var content bytes.Buffer
	content.WriteString("BT\n/F1 12 Tf\n72 720 Td\n")
	for i, ln := range lines {
		if i > 0 {
			content.WriteString("0 -18 Td\n")
		}
		content.WriteString("(" + escapePDF(ln) + ") Tj\n")
	}
	content.WriteString("ET\n")

	streamData := content.Bytes()
	dict := fmt.Sprintf("<< /Length %d >>", len(streamData))
	if compress {
		var buf bytes.Buffer
		w := zlib.NewWriter(&buf)
		w.Write(streamData)
		w.Close()
		streamData = buf.Bytes()
		dict = fmt.Sprintf("<< /Length %d /Filter /FlateDecode >>", len(streamData))
	}

	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	b.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	b.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	b.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>\nendobj\n")
	b.WriteString("4 0 obj\n")
	b.WriteString(dict + "\n")
	b.WriteString("stream\n")
	b.Write(streamData)
	b.WriteString("\nendstream\nendobj\n")
	b.WriteString("trailer\n<< /Root 1 0 R >>\n%%EOF\n")
	return b.Bytes()
}

func escapePDF(s string) string {
	var out strings.Builder
	for _, r := range s {
		switch r {
		case '(', ')', '\\':
			out.WriteByte('\\')
			out.WriteRune(r)
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// 文本型 PDF 应能提取出文字，且保留行结构。
func TestExtractPDFText(t *testing.T) {
	lines := []string{
		"电子发票（普通发票）",
		"发票号码：26157293145980000777",
		"开票日期：2026年11月10日",
		"价税合计（小写）¥73.00",
	}
	pdf := makePDF(lines, false)
	got, err := ExtractPDFText(pdf)
	if err != nil {
		t.Fatalf("应能提取: %v", err)
	}
	for _, want := range []string{"电子发票", "26157293145980000777", "2026年11月10日", "73.00"} {
		if !strings.Contains(got, want) {
			t.Errorf("提取结果缺少 %q\n得到:\n%s", want, got)
		}
	}
	// 行结构要保留：下游按行解析"发票号码：xxx"。
	if !strings.Contains(got, "\n") {
		t.Errorf("应保留换行，实际:\n%s", got)
	}
}

// 压缩过的内容流（FlateDecode）也要能读——真实电子发票基本都是压缩的。
func TestExtractPDFTextCompressed(t *testing.T) {
	lines := []string{
		"电子发票（普通发票）",
		"发票号码：26157293145980001234",
		"开票日期：2026年11月10日",
		"销售方名称：东海铁路客运服务有限公司",
	}
	pdf := makePDF(lines, true)
	got, err := ExtractPDFText(pdf)
	if err != nil {
		t.Fatalf("压缩流应能提取: %v", err)
	}
	if !strings.Contains(got, "26157293145980001234") {
		t.Errorf("压缩流提取失败:\n%s", got)
	}
}

// 没有文本层的 PDF（扫描件）必须明确报错，不能返回空字符串让下游继续。
func TestExtractPDFNoTextLayer(t *testing.T) {
	// 只有图片流，没有文字操作符。
	pdf := []byte("%PDF-1.4\n1 0 obj\n<< /Length 20 >>\nstream\n" +
		"\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\nendstream\nendobj\n%%EOF\n")
	_, err := ExtractPDFText(pdf)
	if err == nil {
		t.Fatal("没有文本层时必须报错，不能假装成功")
	}
	if !strings.Contains(err.Error(), "文本层") {
		t.Errorf("错误信息应说明原因，得到: %v", err)
	}
}

// 加密 PDF 明确拒绝。
func TestExtractPDFEncrypted(t *testing.T) {
	pdf := []byte("%PDF-1.4\n1 0 obj\n<< /Encrypt 9 0 R >>\nstream\n(abc) Tj\nendstream\nendobj\n%%EOF\n")
	_, err := ExtractPDFText(pdf)
	if err == nil || !strings.Contains(err.Error(), "加密") {
		t.Fatalf("加密 PDF 应明确拒绝，得到: %v", err)
	}
}

// 不是 PDF 的文件要认出来。
func TestExtractPDFNotPDF(t *testing.T) {
	_, err := ExtractPDFText([]byte("这就是一段普通文本，不是 PDF"))
	if err != ErrNotPDF {
		t.Errorf("应返回 ErrNotPDF，得到: %v", err)
	}
}

// 发票号码判断：这是审核的命门，识别不出来就不能往下走。
func TestLooksLikeInvoice(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"发票号码：26157293145980000777", true},
		{"发票号: 26157293145980000777", true},
		{"Invoice No: 26157293145980000777", true},
		{"26221257391937633554", true}, // 只有一串 20 位数字
		{"这是一份行程单，没有发票号码", false},
		{"发票", false}, // 有"发票"两个字但没有号码
		{"", false},
	}
	for _, c := range cases {
		if got := looksLikeInvoice(c.text); got != c.want {
			t.Errorf("looksLikeInvoice(%q) = %v，期望 %v", c.text, got, c.want)
		}
	}
}

// 大小限制要真的生效，不能只在文档里写着。
func TestTooLarge(t *testing.T) {
	p := &Processor{Root: t.TempDir()}
	big := make([]byte, MaxBytes+1)
	big[0], big[1], big[2], big[3] = '%', 'P', 'D', 'F'
	_, err := p.Save(Request{InvoiceName: "x.pdf", InvoiceData: big})
	if err == nil {
		t.Fatal("超过上限应报错")
	}
	if !strings.Contains(err.Error(), "上限") {
		t.Errorf("错误应说明大小上限，得到: %v", err)
	}
}

// 图片票据当前不支持，必须明确说"需要 OCR"，而不是静默失败。
func TestImageRejectedWithReason(t *testing.T) {
	p := &Processor{Root: t.TempDir()}
	_, err := p.Save(Request{InvoiceName: "invoice.jpg", InvoiceData: []byte{0xFF, 0xD8, 0xFF, 0xE0}})
	if err == nil {
		t.Fatal("图片应被拒绝")
	}
	if !strings.Contains(err.Error(), "OCR") {
		t.Errorf("应说明需要 OCR，得到: %v", err)
	}
}

// 纯文本票据能正常走完，并落盘成与测试集一致的结构。
func TestSaveTextInvoice(t *testing.T) {
	root := t.TempDir()
	p := &Processor{Root: root}
	txt := "电子发票（普通发票）\n发票号码：26157293145980000777\n开票日期：2026年11月10日\n" +
		"购买方名称：星河科技有限公司\n价税合计（小写）¥73.00\n"
	res, err := p.Save(Request{
		Claim:       sampleClaim(),
		InvoiceName: "invoice.txt",
		InvoiceData: []byte(txt),
	})
	if err != nil {
		t.Fatalf("文本票据应能保存: %v", err)
	}
	if res.Case.HasTruth {
		t.Error("上传的票据没有标准答案，HasTruth 必须为 false")
	}
	if !strings.HasPrefix(res.Case.ID, "UP") {
		t.Errorf("上传单据编号应以 UP 开头，得到 %s", res.Case.ID)
	}
	if res.Case.InvoiceText == "" {
		t.Error("发票文本不能为空")
	}
}

// 纯文本但不是发票（比如一份行程单）：应报"识别不出发票号码"，
// 而不是当成发票继续审。
func TestSaveNonInvoiceText(t *testing.T) {
	p := &Processor{Root: t.TempDir()}
	_, err := p.Save(Request{
		Claim:       sampleClaim(),
		InvoiceName: "x.txt",
		InvoiceData: []byte("行程单\n乘车人：张伟\n2026-11-10 上海到杭州\n票价 73 元\n"),
	})
	if err == nil {
		t.Fatal("没有发票号码的文本不应被当成发票")
	}
	if !strings.Contains(err.Error(), "发票号码") {
		t.Errorf("错误应指出缺少发票号码，得到: %v", err)
	}
}

// 佐证凭证提取失败时，主流程继续，但必须给出警告——不能静默当成已提供。
func TestSupportFailureWarnsButContinues(t *testing.T) {
	p := &Processor{Root: t.TempDir()}
	txt := "电子发票（普通发票）\n发票号码：26157293145980000777\n开票日期：2026年11月10日\n" +
		"购买方名称：星河科技有限公司\n价税合计（小写）¥73.00\n"
	res, err := p.Save(Request{
		Claim:       sampleClaim(),
		InvoiceName: "invoice.txt", InvoiceData: []byte(txt),
		SupportName: "scan.jpg", SupportData: []byte{0xFF, 0xD8, 0xFF},
	})
	if err != nil {
		t.Fatalf("佐证凭证失败不应导致整体失败: %v", err)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("佐证凭证提取失败必须留下警告")
	}
	if res.Case.SupportText != "" {
		t.Error("提取失败时不应写入佐证凭证文本")
	}
}

func sampleClaim() (c schema.Claim) {
	c.ClaimNo = "BX26119999"
	c.RequestNo = "SQ20261117001"
	c.Applicant.Name = "张伟"
	c.Applicant.EmployeeID = "E1001"
	c.Category = "transport"
	c.FromCity = "上海"
	c.City = "杭州"
	c.StartDate = "2026-11-10"
	c.EndDate = "2026-11-11"
	c.Purpose = "客户现场支持"
	return c
}
