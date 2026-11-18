package upload

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"reimburse/plugins/reimburse-audit/judge"
)

// Preview holds editable suggestions for the form. The audit extracts and checks
// invoice fields independently.
type Preview struct {
	InvoiceNo       string   `json:"invoice_no"`
	Category        string   `json:"category,omitempty"`
	AmountFen       *int64   `json:"amount_fen,omitempty"`
	RequestNo       string   `json:"request_no,omitempty"`
	ApplicantName   string   `json:"applicant_name,omitempty"`
	ApplicantID     string   `json:"applicant_id,omitempty"`
	FromCity        string   `json:"from_city,omitempty"`
	City            string   `json:"city,omitempty"`
	StartDate       string   `json:"start_date,omitempty"`
	EndDate         string   `json:"end_date,omitempty"`
	Purpose         string   `json:"purpose,omitempty"`
	EmbeddedSupport bool     `json:"embedded_support,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`
}

var (
	supportHeading = regexp.MustCompile(`(?m)^[ \t]*(?:住宿[ \t]*(?:结账单|水单)(?:[ \t]*[（(]水单[）)])?|酒店[ \t]*(?:水单|结账单)|(?:铁路|航空运输)?电子客票行程单|(?:铁路|航空)?客票行程单|消费清单)[ \t]*$`)
	wrappedTotal   = regexp.MustCompile(`(?m)^合计[ \t]*\n[ \t]*([¥￥][ \t]*[0-9][0-9,.]*)`)
	invoiceTotal   = regexp.MustCompile(`价税合计[^\n¥￥]{0,100}?(?:\n[ \t]*[（(]小写[）)][ \t]*[:：]?[ \t]*)?[¥￥][ \t]*([0-9][0-9,]*)\.([0-9]{2})\b`)
	claimLabels    = map[string]*regexp.Regexp{
		"request": regexp.MustCompile(`(?m)^(?:出差)?申请单号[ \t]*[:：][ \t]*([A-Za-z0-9][A-Za-z0-9_-]{1,49})[ \t]*$`),
		"id":      regexp.MustCompile(`(?m)^申请人工号[ \t]*[:：][ \t]*([A-Za-z0-9][A-Za-z0-9_-]{1,30})[ \t]*$`),
		"name":    regexp.MustCompile(`(?m)^申请人(?:姓名)?[ \t]*[:：][ \t]*([^\n]{1,40})$`),
		"from":    regexp.MustCompile(`(?m)^出发城市[ \t]*[:：][ \t]*([^\n]{1,25})$`),
		"city":    regexp.MustCompile(`(?m)^(?:到达城市|目的城市|住宿城市)[ \t]*[:：][ \t]*([^\n]{1,25})$`),
		"start":   regexp.MustCompile(`(?m)^(?:出差)?开始日期[ \t]*[:：][ \t]*(\d{4}-\d{2}-\d{2})[ \t]*$`),
		"end":     regexp.MustCompile(`(?m)^(?:出差)?结束日期[ \t]*[:：][ \t]*(\d{4}-\d{2}-\d{2})[ \t]*$`),
		"purpose": regexp.MustCompile(`(?m)^(?:出差|报销)?事由[ \t]*[:：][ \t]*([^\n]{1,100})$`),
	}
)

func Inspect(req Request) (Preview, error) {
	invoice, support, embedded, warnings, err := readDocuments(req)
	if err != nil {
		return Preview{}, err
	}
	text := invoice + "\n" + support
	field := func(key string) string {
		matches := claimLabels[key].FindAllStringSubmatch(text, -1)
		var value string
		for _, match := range matches {
			if candidate := strings.TrimSpace(match[1]); value == "" || value == candidate {
				value = candidate
			} else {
				return ""
			}
		}
		return value
	}
	p := Preview{
		InvoiceNo: FindInvoiceNo(invoice), RequestNo: field("request"), ApplicantID: field("id"),
		ApplicantName: field("name"), FromCity: field("from"), City: field("city"), Purpose: field("purpose"),
		StartDate: field("start"), EndDate: field("end"), EmbeddedSupport: embedded, Warnings: warnings,
	}
	for tag, category := range map[string]string{"*运输服务*": "transport", "*住宿服务*": "lodging", "*餐饮服务*": "meal"} {
		if strings.Contains(invoice, tag) {
			if p.Category != "" {
				p.Category = ""
				break
			}
			p.Category = category
		}
	}
	d := judge.ParseSupport(support)
	if p.ApplicantName == "" {
		p.ApplicantName = d.Person
	}
	if p.Category == "lodging" && p.StartDate == "" && p.EndDate == "" && d.Kind == "folio" {
		p.StartDate, p.EndDate = d.CheckIn, d.CheckOut
	}
	p.StartDate, p.EndDate = validDate(p.StartDate), validDate(p.EndDate)
	if p.EndDate != "" && p.StartDate > p.EndDate {
		p.StartDate, p.EndDate = "", ""
	}
	if matches := invoiceTotal.FindAllStringSubmatch(invoice, -1); len(matches) == 1 {
		digits := strings.ReplaceAll(matches[0][1], ",", "") + matches[0][2]
		if amount, err := strconv.ParseInt(digits, 10, 64); err == nil {
			p.AmountFen = &amount
		}
	}
	return p, nil
}

func validDate(value string) string {
	if _, err := time.Parse(time.DateOnly, value); err != nil {
		return ""
	}
	return value
}

func readDocuments(req Request) (invoice, support string, embedded bool, warnings []string, err error) {
	if len(req.InvoiceData) == 0 {
		return "", "", false, nil, fmt.Errorf("%w：发票文件", ErrEmpty)
	}
	if len(req.InvoiceData) > MaxBytes || len(req.SupportData) > MaxSupportBytes {
		return "", "", false, nil, ErrTooLarge
	}
	invoice, err = extract(req.InvoiceName, req.InvoiceData, KindInvoice)
	if err != nil {
		return "", "", false, nil, fmt.Errorf("发票：%w", err)
	}
	var bundled string
	if IsPDF(req.InvoiceData) {
		if loc := supportHeading.FindStringIndex(invoice); loc != nil && loc[0] > 0 {
			candidate := wrappedTotal.ReplaceAllString(strings.TrimSpace(invoice[loc[0]:]), "合计：$1")
			doc := judge.ParseSupport(candidate)
			if doc.HasTotal && (doc.Date != "" || doc.CheckIn != "" && doc.CheckOut != "") {
				bundled = candidate
				invoice = strings.TrimSpace(invoice[:loc[0]])
			}
		}
	}
	if !looksLikeInvoice(invoice) {
		return "", "", false, nil, ErrNoInvoiceNo
	}
	if len(req.SupportData) > 0 {
		support, err = extract(req.SupportName, req.SupportData, KindSupport)
		if err != nil {
			warnings = append(warnings, "另传的佐证凭证无法提取文本："+err.Error())
		} else if loc := supportHeading.FindStringIndex(support); loc != nil {
			support = strings.TrimSpace(support[loc[0]:])
		}
	}
	if support == "" && bundled != "" {
		support, embedded = bundled, true
	}
	return invoice, wrappedTotal.ReplaceAllString(support, "合计：$1"), embedded, warnings, nil
}
