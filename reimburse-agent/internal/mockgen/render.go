package mockgen

import (
	"fmt"
	"math/rand"
	"strings"

	"reimburse/internal/schema"
)

const blockCh = "█"

// smudge 模拟污损：保留首尾各 keep 个字符，中间全部变成方块。
func smudge(s string, keep int) string {
	rs := []rune(s)
	if len(rs) <= keep*2 {
		return strings.Repeat(blockCh, len(rs))
	}
	var b strings.Builder
	for i, r := range rs {
		if i < keep || i >= len(rs)-keep {
			b.WriteRune(r)
		} else {
			b.WriteString(blockCh)
		}
	}
	return b.String()
}

// smudgeMoney 把金额里的数字全部变成方块，保留货币符号和小数点。
func smudgeMoney(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteString(blockCh)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func cnDate(s string) string {
	t := mustDate(s)
	return fmt.Sprintf("%d年%02d月%02d日", t.Year(), int(t.Month()), t.Day())
}

// renderInvoice 把发票结构化数据渲染成类 OCR 文本。unreadable 中的关键字段会被污损。
func renderInvoice(inv schema.Invoice, unreadable []string) string {
	un := setOf(unreadable)
	no, date := inv.InvoiceNo, cnDate(inv.IssueDate)
	buyerTax, sellerTax := inv.BuyerTaxID, inv.SellerTaxID
	total, ex, tax := "¥"+yuanStr(inv.TotalFen), "¥"+yuanStr(inv.ExTaxFen), "¥"+yuanStr(inv.TaxFen)
	big := daxie(inv.TotalFen)
	if un["invoice_no"] {
		no = smudge(no, 2)
	}
	if un["issue_date"] {
		date = "2026年██月██日"
	}
	if un["buyer_tax_id"] {
		buyerTax = smudge(buyerTax, 3)
	}
	if un["seller_tax_id"] {
		sellerTax = smudge(sellerTax, 3)
	}
	if un["total_amount"] {
		total, ex, tax = smudgeMoney(total), smudgeMoney(ex), smudgeMoney(tax)
		big = strings.Repeat(blockCh, 5)
	}
	var b strings.Builder
	b.WriteString("电子发票（普通发票）\n")
	fmt.Fprintf(&b, "发票号码：%s\n", no)
	fmt.Fprintf(&b, "开票日期：%s\n", date)
	b.WriteString("购买方信息\n")
	fmt.Fprintf(&b, "名称：%s\n", inv.BuyerName)
	fmt.Fprintf(&b, "统一社会信用代码/纳税人识别号：%s\n", buyerTax)
	b.WriteString("销售方信息\n")
	fmt.Fprintf(&b, "名称：%s\n", inv.SellerName)
	fmt.Fprintf(&b, "统一社会信用代码/纳税人识别号：%s\n", sellerTax)
	b.WriteString("项目名称  金额  税率/征收率  税额\n")
	fmt.Fprintf(&b, "%s  %s  %d%%  %s\n", inv.ItemName, ex, inv.TaxRatePct, tax)
	fmt.Fprintf(&b, "合计  %s  %s\n", ex, tax)
	fmt.Fprintf(&b, "价税合计（大写）：%s    （小写）%s\n", big, total)
	if inv.Remark != "" {
		fmt.Fprintf(&b, "备注：%s\n", inv.Remark)
	}
	b.WriteString("开票人：系统自动开具\n")
	return b.String()
}

// renderSupport 渲染行程单、水单或消费清单。
func renderSupport(s *schema.Support) string {
	var b strings.Builder
	switch s.Kind {
	case "itinerary":
		if s.Mode == "flight" {
			b.WriteString("航空运输电子客票行程单\n")
			fmt.Fprintf(&b, "旅客姓名：%s\n", s.Person)
			fmt.Fprintf(&b, "航班号：%s\n", s.Vehicle)
			fmt.Fprintf(&b, "乘机日期：%s\n", s.Date)
			fmt.Fprintf(&b, "航程：%s - %s\n", s.From, s.To)
			fmt.Fprintf(&b, "舱位：%s\n", s.Seat)
		} else {
			b.WriteString("铁路电子客票报销凭证\n")
			fmt.Fprintf(&b, "乘车人：%s\n", s.Person)
			fmt.Fprintf(&b, "乘车日期：%s\n", s.Date)
			fmt.Fprintf(&b, "车次：%s  %s站 → %s站\n", s.Vehicle, s.From, s.To)
			fmt.Fprintf(&b, "席别：%s\n", s.Seat)
		}
		for _, l := range s.Lines {
			fmt.Fprintf(&b, "%s：¥%s\n", l.Name, yuanStr(l.AmountFen))
		}
		fmt.Fprintf(&b, "合计：¥%s\n", yuanStr(s.TotalFen))
	case "folio":
		b.WriteString("住宿结账单（水单）\n")
		fmt.Fprintf(&b, "酒店：%s\n", s.Hotel)
		fmt.Fprintf(&b, "入住人：%s\n", s.Person)
		fmt.Fprintf(&b, "入住日期：%s\n", s.CheckIn)
		fmt.Fprintf(&b, "离店日期：%s\n", s.CheckOut)
		fmt.Fprintf(&b, "共 %d 晚\n", s.Nights)
		b.WriteString("逐日房费：\n")
		for _, l := range s.Lines {
			fmt.Fprintf(&b, "%s  %s  ¥%s\n", l.Date, l.Name, yuanStr(l.AmountFen))
		}
		fmt.Fprintf(&b, "合计：¥%s\n", yuanStr(s.TotalFen))
	case "consumption":
		b.WriteString("消费清单\n")
		fmt.Fprintf(&b, "商户：%s\n", s.Venue)
		fmt.Fprintf(&b, "消费日期：%s\n", s.Date)
		b.WriteString("品名  数量  金额\n")
		for _, l := range s.Lines {
			fmt.Fprintf(&b, "%s  x%d  ¥%s\n", l.Name, l.Qty, yuanStr(l.AmountFen))
		}
		fmt.Fprintf(&b, "合计：¥%s\n", yuanStr(s.TotalFen))
	}
	return b.String()
}

// noisify 模拟 OCR 排版噪声：冒号半角化、多余空格、行尾空白。不改动任何数字和文字内容。
func noisify(r *rand.Rand, text string) string {
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		if ln == "" {
			continue
		}
		if r.Float64() < 0.12 {
			ln = strings.Replace(ln, "：", ":", 1)
		}
		if r.Float64() < 0.10 {
			ln = strings.Replace(ln, "：", "：  ", 1)
		}
		if r.Float64() < 0.05 {
			ln += "  "
		}
		lines[i] = ln
	}
	return strings.Join(lines, "\n")
}

// Render 生成用例的发票文本和佐证凭证文本。噪声用按用例编号派生的随机源，增删用例不影响其他用例。
func (g *Gen) Render(c *schema.Case, idx int) {
	r := rand.New(rand.NewSource(g.seed*1000 + int64(idx)))
	c.InvoiceText = noisify(r, renderInvoice(c.Truth.Invoice, c.Truth.Unreadable))
	if c.Truth.Support != nil {
		c.SupportText = noisify(r, renderSupport(c.Truth.Support))
		c.Claim.Attachments.SupportFile = "support.txt"
	} else {
		c.SupportText = ""
		c.Claim.Attachments.SupportFile = ""
	}
}
