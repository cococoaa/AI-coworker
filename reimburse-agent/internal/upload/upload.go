// Package upload 接收真实票据，把它变成可审核的单据。
//
// 设计要点：
//
//  1. **上传不等于可信。** 上传的文件只提供"文本内容"，不提供结论。
//     文本提取失败时必须明确失败，不能拿空文本当"票据是空的"继续跑——
//     那会得出一个"看起来合规"的结论，比直接报错危险得多。
//
//  2. **单据要落盘。** 上传后立刻写成用例目录（claim.json + invoice.txt），
//     与测试集同一套结构。这样审核、任务、查重、重启不丢全部自然复用，
//     不需要为"上传的单"另写一条链路。
//
//  3. **没有标准答案。** 上传的单据没有 truth.json，HasTruth=false。
//     合成演示模式会因此拒绝它——这是刻意的。
//
//  4. **大小与类型受限。** 只接受 PDF 与纯文本；单文件上限 8 MB。
//     图片型票据（扫描件）当前不支持，会返回明确的错误而不是静默降级。
package upload

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"reimburse/internal/schema"
)

// 限制。
const (
	// MaxBytes 是单个文件上限（8 MB）。一张发票 PDF 通常几百 KB，
	// 8 MB 足够容纳多页扫描件；再大基本是上传错了文件。
	MaxBytes = 8 << 20
	// MaxSupportBytes 是佐证凭证上限（4 MB）。
	MaxSupportBytes = 4 << 20
)

// Kind 是上传文件的类型。
type Kind string

const (
	KindInvoice Kind = "invoice" // 发票
	KindSupport Kind = "support" // 佐证凭证（行程单 / 水单 / 消费清单）
)

// 错误分类。调用方据此决定给用户什么提示、要不要转人工。
var (
	// ErrTooLarge 文件超限。
	ErrTooLarge = errors.New("文件超过大小上限")
	// ErrUnsupported 不支持的文件类型。
	ErrUnsupported = errors.New("不支持的文件类型")
	// ErrEmpty 文件为空。
	ErrEmpty = errors.New("文件内容为空")
	// ErrNoInvoiceNo 没能从发票里读出号码——没有号码就查不了重、验不了真。
	ErrNoInvoiceNo = errors.New("未能从发票中识别出发票号码")
)

// Request 是一次上传请求。
type Request struct {
	// Claim 是报销单（由界面表单填写）。上传只负责票据文本。
	Claim schema.Claim
	// InvoiceName / InvoiceData 是发票文件。
	InvoiceName string
	InvoiceData []byte
	// SupportName / SupportData 是佐证凭证（可选）。
	SupportName string
	SupportData []byte
}

// Result 是上传结果。
type Result struct {
	// Case 是可直接送审的单据。
	Case *schema.Case
	// Warnings 是"能跑但要知道"的提示，例如佐证凭证提取失败。
	Warnings []string
}

// Processor 负责把上传内容落成用例目录。
type Processor struct {
	// Root 是数据根目录，上传的单据写到 <Root>/uploads/<ID>。
	Root string
	// Now 便于测试注入时间。
	Now func() time.Time
}

// Save 处理一次上传：提取文本、校验、落盘、返回可审单据。
//
// 返回值里的 Case 已经可以直接交给 Server 审核——它与测试集里的用例
// 结构完全一致（除 truth 为空）。
func (p *Processor) Save(req Request) (*Result, error) {
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}

	if len(req.InvoiceData) == 0 {
		return nil, fmt.Errorf("%w：发票文件", ErrEmpty)
	}
	if len(req.InvoiceData) > MaxBytes {
		return nil, fmt.Errorf("%w：发票 %d 字节，上限 %d", ErrTooLarge, len(req.InvoiceData), MaxBytes)
	}

	invoiceText, err := extract(req.InvoiceName, req.InvoiceData, KindInvoice)
	if err != nil {
		return nil, fmt.Errorf("发票：%w", err)
	}

	// 发票号是审核的命门：没有它，查验与查重都做不了。
	// 这里只检查"能否看出号码"，不解析整张票——字段提取交给模型。
	if !looksLikeInvoice(invoiceText) {
		return nil, fmt.Errorf("%w（提取到的文本里没有可识别的发票号码）", ErrNoInvoiceNo)
	}

	res := &Result{}

	var supportText string
	if len(req.SupportData) > 0 {
		if len(req.SupportData) > MaxSupportBytes {
			return nil, fmt.Errorf("%w：佐证凭证 %d 字节，上限 %d",
				ErrTooLarge, len(req.SupportData), MaxSupportBytes)
		}
		supportText, err = extract(req.SupportName, req.SupportData, KindSupport)
		if err != nil {
			// 佐证凭证读不出来时继续跑：主判定仍能做，缺件由规则层按"缺材料"处理。
			// 但不能静默——要在结果里说清楚，否则用户以为凭证已经算进去了。
			res.Warnings = append(res.Warnings,
				"佐证凭证无法提取文本（"+err.Error()+"），本次按「未提供佐证凭证」处理")
			supportText = ""
		}
	}

	// 组装单据。
	cl := req.Claim
	cl.Attachments.InvoiceFile = "invoice.txt"
	if supportText != "" {
		cl.Attachments.SupportFile = "support.txt"
	}
	id := caseID(now())
	c := &schema.Case{
		ID:          id,
		Claim:       cl,
		InvoiceText: invoiceText,
		SupportText: supportText,
		HasTruth:    false, // 上传的票据没有标准答案
	}

	if err := p.write(c); err != nil {
		return nil, err
	}
	res.Case = c
	return res, nil
}

// extract 按扩展名/内容挑选提取方式。
func extract(name string, data []byte, kind Kind) (string, error) {
	ext := strings.ToLower(filepath.Ext(name))

	// 先按内容判断：PDF 有魔数，不依赖扩展名（用户常把 PDF 存成 .txt）。
	if IsPDF(data) {
		txt, err := ExtractPDFText(data)
		if err != nil {
			return "", err
		}
		return txt, nil
	}

	switch ext {
	case ".txt", ".text", ".md", "":
		if !utf8.Valid(data) {
			return "", fmt.Errorf("%w：纯文本文件不是 UTF-8 编码", ErrUnsupported)
		}
		txt := normalizeWhitespace(string(data))
		if strings.TrimSpace(txt) == "" {
			return "", fmt.Errorf("%w：文本文件没有内容", ErrEmpty)
		}
		return txt, nil
	case ".pdf":
		// 扩展名是 pdf 但内容不是：按内容为准，报清楚。
		return "", fmt.Errorf("%w：扩展名是 .pdf 但文件内容不是 PDF", ErrUnsupported)
	case ".jpg", ".jpeg", ".png", ".webp", ".bmp", ".tif", ".tiff":
		return "", fmt.Errorf("%w：图片票据需要 OCR，当前版本不支持；"+
			"请上传电子发票的 PDF 或文本，或走人工录入", ErrUnsupported)
	}
	return "", fmt.Errorf("%w：%s（只支持 PDF 与纯文本）", ErrUnsupported, ext)
}

// IsPDF 判断内容是否为 PDF（看魔数，不看扩展名）。
func IsPDF(b []byte) bool {
	return len(b) >= 4 && b[0] == '%' && b[1] == 'P' && b[2] == 'D' && b[3] == 'F'
}

// invoiceNoPattern 匹配"发票号码：123..."这类写法。
//
// 允许全角冒号、可有空格、号码至少 8 位数字。不追求精确校验（那是模型与规则的事），
// 只用来判断"这份文本看起来是不是发票"。
var invoiceNoPattern = regexp.MustCompile(`(?:发票号码|发票号|Invoice\s*No)[^0-9]{0,6}([0-9]{8,})`)

// looksLikeInvoice 判断提取出的文本是否有发票的基本特征。
func looksLikeInvoice(text string) bool {
	if invoiceNoPattern.MatchString(text) {
		return true
	}
	// 有些电子发票把号码写成 "No:12345678" 或直接一串 20 位数字。
	if regexp.MustCompile(`\b[0-9]{20}\b`).MatchString(text) {
		return true
	}
	return strings.Contains(text, "发票") && regexp.MustCompile(`[0-9]{8,}`).MatchString(text)
}

// caseID 生成单据编号：上传的单据用 UP 前缀，与测试集 C 前缀区分开。
// 带时间戳，避免同一秒多次上传互相覆盖。
func caseID(t time.Time) string {
	return "UP" + t.Format("20060102150405")
}

// write 把单据写成用例目录。
func (p *Processor) write(c *schema.Case) error {
	dir := filepath.Join(p.Root, "uploads", c.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建上传目录失败: %w", err)
	}
	if err := schema.WriteJSON(filepath.Join(dir, "claim.json"), c.Claim); err != nil {
		return fmt.Errorf("写入报销单失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "invoice.txt"), []byte(c.InvoiceText), 0o644); err != nil {
		return fmt.Errorf("写入发票文本失败: %w", err)
	}
	if c.SupportText != "" {
		if err := os.WriteFile(filepath.Join(dir, "support.txt"), []byte(c.SupportText), 0o644); err != nil {
			return fmt.Errorf("写入佐证凭证失败: %w", err)
		}
	}
	// 刻意不写 truth.json / world.json：上传的票据没有标准答案，
	// 核验工具也查不到它的数据（会返回"查无"）。这符合真实情况。
	return nil
}

// FindInvoiceNo 从文本里取出第一个发票号码，取不到返回空串。
// 展示用；判定仍以模型抽取与规则为准。
func FindInvoiceNo(text string) string {
	m := invoiceNoPattern.FindStringSubmatch(text)
	if len(m) >= 2 {
		return m[1]
	}
	// 退回：找 20 位连续数字。
	if loc := regexp.MustCompile(`[0-9]{20}`).FindString(text); loc != "" {
		return loc
	}
	return ""
}
