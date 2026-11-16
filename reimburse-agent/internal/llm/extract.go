package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// 抽取字段名。与 rules.json 中的 key_fields 对应，少一个都会导致对应规则不作判定。
const (
	FInvoiceNo   = "invoice_no"
	FIssueDate   = "issue_date"
	FTotalAmount = "total_amount"
	FBuyerTaxID  = "buyer_tax_id"
	FSellerTaxID = "seller_tax_id"
	FBuyerName   = "buyer_name"
	FSellerName  = "seller_name"
	FItemName    = "item_name"
)

// ExtractFields 是发票抽取涉及的全部字段，顺序固定，便于界面和报告稳定输出。
var ExtractFields = []string{
	FInvoiceNo, FIssueDate, FTotalAmount, FBuyerTaxID, FSellerTaxID,
	FBuyerName, FSellerName, FItemName,
}

// FieldValue 是一个抽取字段的值与置信度。
// Value 一律为字符串：金额为 "553.00"（元，两位小数），日期为 YYYY-MM-DD。
// 这样两个模型的原始输出可以直接逐字比对，不一致就意味着真的不一致，而不是格式差异。
type FieldValue struct {
	Value string  `json:"value"`
	Conf  float64 `json:"confidence"`
}

// Missing 表示该字段模型明确表示无法辨认。
func (f FieldValue) Missing() bool { return f.Value == "" }

// Extraction 是一次抽取的结果。
type Extraction struct {
	Provider  string                `json:"provider"`
	Model     string                `json:"model"`
	Fields    map[string]FieldValue `json:"fields"`
	Notes     string                `json:"notes,omitempty"`
	Usage     Usage                 `json:"usage"`
	LatencyMS int64                 `json:"latency_ms"`
	Attempts  int                   `json:"attempts"`
}

// Source 描述要抽取的文档。
type Source struct {
	Category    string // transport / lodging / meal
	InvoiceText string
	SupportText string // 可能为空
}

// extractSchema 是两家模型共用的 JSON Schema。
// Anthropic 的结构化输出不支持递归，这里全是扁平字段，可以用同一份。
func extractSchema() map[string]any {
	fields := map[string]any{
		"type":        "object",
		"description": "单个字段的抽取结果",
		"properties": map[string]any{
			"value": map[string]any{
				"type":        "string",
				"description": "字段值。原文被污损、遮挡或看不清楚时，只填能看清的部分，并在 notes 说明；完全看不清填空字符串。",
			},
			"confidence": map[string]any{
				"type":        "number",
				"description": "对该字段抽取正确的把握，0 到 1。原文清晰无误应接近 1；有任何遮挡、模糊、需推测应低于 0.8。",
			},
		},
		"required":             []string{"value", "confidence"},
		"additionalProperties": false,
	}
	props := map[string]any{}
	for _, f := range ExtractFields {
		props[f] = fields
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"fields": props,
			"notes": map[string]any{
				"type":        "string",
				"description": "无法辨认或有歧义的内容说明，没有则填空字符串。",
			},
		},
		"required": []string{"fields", "notes"},
	}
}

const extractSystem = `你是财务共享中心的票据录入员。任务是把一张增值税电子发票（必要时连同佐证凭证）上的指定字段抄录成 JSON，供后续规则审核使用。

要求：
1. 只抄录原文，不做推断、补全或纠正。看不清的字符用空字符串代替，并在 notes 中写明是哪个字段。
2. 金额一律填成以元为单位的十进制字符串，保留两位小数，不带货币符号和千分位，例如 "553.00"。
3. 日期一律填成 YYYY-MM-DD，例如 "2026-10-01"。原文若为"2026年10月1日"，也转成该格式。
4. 税号照抄 18 位，不要纠正大小写或补位。
5. confidence 是你的真实把握：原文清晰为 1；有污损、遮挡、模糊，或需要结合上下文才能确定，必须低于 0.8。
6. 只输出 json，不要输出任何其他文字。输出格式示例：
{"fields":{"invoice_no":{"value":"26123456789012345678","confidence":1},"issue_date":{"value":"2026-10-01","confidence":1},"total_amount":{"value":"553.00","confidence":1},"buyer_tax_id":{"value":"91310115MA1FL8X3K0","confidence":1},"seller_tax_id":{"value":"91310103692B6R5G35","confidence":1},"buyer_name":{"value":"星河科技有限公司","confidence":1},"seller_name":{"value":"东海铁路客运服务有限公司","confidence":1},"item_name":{"value":"*运输服务*铁路旅客运输","confidence":1}},"notes":""}`

func extractUserPrompt(src Source) string {
	s := fmt.Sprintf("报销类别：%s\n\n【发票原文】\n%s", categoryCN(src.Category), src.InvoiceText)
	if src.SupportText != "" {
		s += "\n\n【佐证凭证原文】\n" + src.SupportText +
			"\n\n说明：佐证凭证用于核对日期、姓名等，但上面的字段必须以发票为准；只有发票完全无法辨认某字段时，才参考佐证凭证，并把该字段的 confidence 压到 0.5 以下。"
	}
	return s
}

func categoryCN(c string) string {
	switch c {
	case "transport":
		return "交通（火车或飞机）"
	case "lodging":
		return "住宿"
	case "meal":
		return "餐饮（工作餐或业务招待）"
	}
	return c
}

// ExtractPrompt 返回抽取用的 System Prompt、用户消息与 JSON Schema，
// 供录制工具构造完全一致的请求（与线上调用同一份 Prompt，避免录制与回放不一致）。
func ExtractPrompt(src Source) (system, user string, sch map[string]any) {
	return extractSystem, extractUserPrompt(src), extractSchema()
}

// rawExtraction 是响应体的反序列化目标。
type rawExtraction struct {
	Fields map[string]struct {
		Value      string   `json:"value"`
		Confidence *float64 `json:"confidence"`
	} `json:"fields"`
	Notes string `json:"notes"`
}

// parseExtraction 把模型文本解析为 Extraction，并做结构与类型校验。
// 缺字段、多字段、置信度越界、值为非字符串，一律判为 bad_json。
func parseExtraction(text string) (*Extraction, *SchemaIssue) {
	b, err := ExtractJSON(text)
	if err != nil {
		e := err.(*Error)
		return nil, &SchemaIssue{Kind: e.Kind, Detail: e.Message}
	}
	var raw rawExtraction
	dec := json.NewDecoder(bytesReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, &SchemaIssue{Kind: ErrBadJSON, Detail: "字段结构不符: " + err.Error()}
	}
	ex := &Extraction{Fields: map[string]FieldValue{}, Notes: raw.Notes}
	for _, f := range ExtractFields {
		v, ok := raw.Fields[f]
		if !ok {
			ex.Fields[f] = FieldValue{Value: "", Conf: 0}
			continue
		}
		conf := 0.0
		if v.Confidence != nil {
			conf = *v.Confidence
		}
		if conf < 0 || conf > 1 {
			return nil, &SchemaIssue{Kind: ErrBadJSON, Detail: fmt.Sprintf("%s 的 confidence 越界: %v", f, conf)}
		}
		ex.Fields[f] = FieldValue{Value: v.Value, Conf: conf}
	}
	return ex, nil
}

// SchemaIssue 是 Schema 校验失败的原因。
type SchemaIssue struct {
	Kind   ErrKind
	Detail string
}

// Extract 用指定供应商抽取一张单据。返回的 Extraction 在失败时为 nil，原因在 SchemaIssue 里。
func (c *Client) Extract(ctx context.Context, providerName string, src Source) (*Extraction, *SchemaIssue) {
	return c.ExtractWith(ctx, providerName, src, nil)
}

// ExtractWith 抽取并回调。onCall 可以接收每次调用的原始响应，供界面实时展示。
func (c *Client) ExtractWith(ctx context.Context, providerName string, src Source, onCall func(*Response)) (*Extraction, *SchemaIssue) {
	res := c.Call(ctx, providerName, &Request{
		System:     extractSystem,
		Messages:   []Message{{Role: "user", Content: extractUserPrompt(src)}},
		Schema:     extractSchema(),
		SchemaName: "invoice_extraction",
	})
	if res.Err != nil {
		e := res.Err.(*Error)
		return nil, &SchemaIssue{Kind: e.Kind, Detail: e.Error()}
	}
	if onCall != nil {
		onCall(res.Resp)
	}
	ex, issue := parseExtraction(res.Resp.Text)
	if issue != nil {
		return nil, issue
	}
	ex.Provider, ex.Model = res.Resp.Provider, res.Resp.Model
	ex.Usage, ex.LatencyMS, ex.Attempts = res.Resp.Usage, res.Resp.LatencyMS, res.Resp.Attempts
	return ex, nil
}

// DualExtract 用两家模型并行抽取同一张单据，再逐字段比对。
// 两边都成功才进入比对；一边失败，结果里保留失败原因，由上层转人工。
func (c *Client) DualExtract(ctx context.Context, src Source) *DualResult {
	var wg sync.WaitGroup
	out := &DualResult{Fields: map[string]FieldValue{}}
	names := []string{ProviderDeepSeek, ProviderClaude}
	exs := make([]*Extraction, len(names))
	issues := make([]*SchemaIssue, len(names))
	for i, n := range names {
		wg.Add(1)
		go func(i int, n string) {
			defer wg.Done()
			exs[i], issues[i] = c.Extract(ctx, n, src)
		}(i, n)
	}
	wg.Wait()
	out.A, out.IssueA = exs[0], issues[0]
	out.B, out.IssueB = exs[1], issues[1]
	out.compare()
	return out
}

// DualResult 是双模型抽取的合并结果。
type DualResult struct {
	A              *Extraction  `json:"a"`
	B              *Extraction  `json:"b"`
	IssueA, IssueB *SchemaIssue `json:"-"`
	// Fields 是合并后的字段：两边一致时取 A 的值、置信度取较小者；
	// 不一致或只有一边成功时，该字段标记为不可信（Conf 为 0 且 Untrusted 为真）。
	Fields    map[string]FieldValue `json:"fields"`
	Untrusted []string              `json:"untrusted"`
	Disagree  []string              `json:"disagree"`
	LowConf   []string              `json:"low_confidence"`
	BothOK    bool                  `json:"both_ok"`
	Degraded  string                `json:"degraded,omitempty"`
}

// Compare 供编排层在自行并发调用后合并结果时使用。
func (d *DualResult) Compare() { d.compare() }

// Compare 逐字段比对。比对口径：去空白、税号转大写、金额规范化到两位小数。
// 这类归一化只消除格式差异，不消除数值差异。
func (d *DualResult) compare() {
	if d.Fields == nil {
		d.Fields = map[string]FieldValue{}
	}
	d.BothOK = d.A != nil && d.B != nil
	switch {
	case d.A != nil && d.B == nil:
		d.Degraded = "只有 deepseek 抽取成功"
	case d.A == nil && d.B != nil:
		d.Degraded = "只有 claude 抽取成功"
	}
	for _, f := range ExtractFields {
		var va, vb FieldValue
		if d.A != nil {
			va = d.A.Fields[f]
		}
		if d.B != nil {
			vb = d.B.Fields[f]
		}
		switch {
		case d.A != nil && d.B != nil:
			na, nb := normalizeValue(f, va.Value), normalizeValue(f, vb.Value)
			if na != nb {
				d.Disagree = append(d.Disagree, f)
				d.Untrusted = append(d.Untrusted, f)
				d.Fields[f] = FieldValue{Value: "", Conf: 0}
				continue
			}
			conf := va.Conf
			if vb.Conf < conf {
				conf = vb.Conf
			}
			d.Fields[f] = FieldValue{Value: na, Conf: conf}
			if conf < LowThreshold {
				d.LowConf = append(d.LowConf, f)
			}
		case d.A != nil:
			d.Fields[f] = FieldValue{Value: normalizeValue(f, va.Value), Conf: va.Conf}
		case d.B != nil:
			d.Fields[f] = FieldValue{Value: normalizeValue(f, vb.Value), Conf: vb.Conf}
		default:
			d.Fields[f] = FieldValue{}
		}
	}
}

// LowThreshold 与规则表中的 low_confidence_threshold 保持一致。
const LowThreshold = 0.8

// normalizeValue 只做格式归一化：金额补齐两位小数，税号转大写，日期去空格。
func normalizeValue(field, v string) string {
	switch field {
	case FTotalAmount:
		var f float64
		if _, err := fmt.Sscanf(trimSpace(v), "%g", &f); err == nil {
			return fmt.Sprintf("%.2f", f)
		}
	case FBuyerTaxID, FSellerTaxID, FInvoiceNo:
		return upper(trimSpace(v))
	case FIssueDate:
		return trimSpace(v)
	}
	return trimSpace(v)
}
