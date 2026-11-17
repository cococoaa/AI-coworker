package audit

import (
	"fmt"
	"strings"

	"reimburse/plugins/reimburse-audit/judge"
)

// 字段溯源：每个抽出的值，回到原件里指出它出自哪一句。
//
// 为什么值得做：财务复核时最常问的一句是"这个数你从哪儿看的"。
// 只说"模型抽的、置信度 0.95"没有用——置信度是模型自己说的，不是证据。
// 所以这里做一件很朴素的事：把值拿回原件文本里找，找到就把上下文摘出来。
//
// 三种结果，含义完全不同：
//
//	found    原件里确实有这串字符 → 可以核对
//	support  发票上找不到，只见于佐证凭证 → 危险信号：字段本该以发票为准
//	missing  两处都找不到 → 可能是模型补全/改写，也可能是格式差异（如金额千分位）
//
// 注意：missing 不等于错。模型把 "¥1,234.00" 抄成 "1234.00" 是合规的，
// 所以这里只报"未找到"，不擅自判定为幻觉；判定仍由规则表负责。
// 但复核人应该看到这个标记。

// FieldTrace 是一个字段的溯源结果。
type FieldTrace struct {
	// Field 是字段名（与抽取层一致）。
	Field string `json:"field"`
	// Label 是中文名，界面直接显示。
	Label string `json:"label"`
	// Value 是合并后的取值。
	Value string `json:"value"`
	// Conf 是合并后的置信度。
	Conf float64 `json:"confidence"`
	// Status 是字段四态（present/absent/unreadable/ambiguous/unknown）。
	Status string `json:"status"`
	// Source 取 invoice / support / none / empty。
	Source string `json:"source"`
	// Snippet 是原件里命中的上下文（截取前后若干字）。
	Snippet string `json:"snippet,omitempty"`
	// Note 是一句话说明，给财务看的。
	Note string `json:"note,omitempty"`
}

// 溯源结果的各种来源。
const (
	TraceInvoice = "invoice"
	TraceSupport = "support"
	TraceNone    = "none"
	TraceEmpty   = "empty"
)

// TraceFields 为报告里的每个字段生成溯源信息。
//
// invoiceText / supportText 是送进模型的那两份原文（补件续审时是补交后的版本），
// 所以这里找到的位置与模型当时看到的一致。
func TraceFields(rep *Report, invoiceText, supportText string) []FieldTrace {
	out := make([]FieldTrace, 0, len(judge.FieldNames()))
	for _, name := range judge.FieldNames() {
		t := FieldTrace{Field: string(name), Label: judge.FieldCN(name)}
		if rep.Extract != nil {
			if fv, ok := rep.Extract.Fields[string(name)]; ok {
				t.Value, t.Conf = fv.Value, fv.Conf
			}
		}
		if rep.Result != nil && rep.Result.FieldStatus != nil {
			t.Status = rep.Result.FieldStatus.Of(name).String()
		}
		if t.Status == "" {
			t.Status = judge.Unknown.String()
		}

		if strings.TrimSpace(t.Value) == "" {
			t.Source = TraceEmpty
			switch t.Status {
			case judge.Absent.String():
				t.Note = "原件该栏为空（不是识别失败）"
			case judge.Unreadable.String():
				t.Note = "该栏看不清，未能读出"
			case judge.Ambiguous.String():
				t.Note = "两个模型结果不一致，未取信"
			default:
				t.Note = "未读到值"
			}
			out = append(out, t)
			continue
		}

		if snippet, ok := locate(invoiceText, name, t.Value); ok {
			t.Source, t.Snippet = TraceInvoice, snippet
			out = append(out, t)
			continue
		}
		// 发票上没找到，再看佐证凭证。
		if snippet, ok := locate(supportText, name, t.Value); ok {
			t.Source, t.Snippet = TraceSupport, snippet
			t.Note = "发票原文中未找到，只见于佐证凭证；该字段应以发票为准"
			out = append(out, t)
			continue
		}
		t.Source = TraceNone
		t.Note = suspiciousNote(name)
		out = append(out, t)
	}
	return out
}

// suspiciousNote 说明"没找到"可能是什么原因，避免一上来就扣帽子。
func suspiciousNote(name judge.FieldName) string {
	switch name {
	case judge.FTotalAmount:
		return "原件中未逐字找到该金额（可能原件带千分位或货币符号），建议人工核对金额"
	case judge.FIssueDate:
		return "原件中未找到该日期（可能原件用「2026年11月1日」写法），建议人工核对日期"
	default:
		return "原件中未找到该值，可能是模型补全或格式改写，建议人工核对"
	}
}

// locate 在原文里找 value，返回带上下文的片段。
//
// 匹配方式按字段类型放宽，但只放宽"排版差异"，不放宽"内容差异"：
//   - 金额：允许千分位、"¥"前缀、末尾 .00 省略
//   - 日期：允许 2026-11-01 / 2026/11/01 / 2026年11月1日 互相转换
//   - 其余：允许中间有空格、大小写不同
func locate(text string, name judge.FieldName, value string) (string, bool) {
	if strings.TrimSpace(text) == "" || strings.TrimSpace(value) == "" {
		return "", false
	}
	squash := squashSpaces(text)

	for _, cand := range candidates(name, value) {
		if cand == "" {
			continue
		}
		if idx := strings.Index(squash, squashSpaces(cand)); idx >= 0 {
			return excerpt(squash, idx, len(squashSpaces(cand))), true
		}
	}
	return "", false
}

// candidates 列出一个值在原件里可能的几种写法。
func candidates(name judge.FieldName, value string) []string {
	v := strings.TrimSpace(value)
	out := []string{v}
	switch name {
	case judge.FTotalAmount:
		// 553.00 → 553.00 / ¥553.00 / 553 / 553.0 / 55300(分) 之外的常见写法
		out = append(out, "¥"+v, "￥"+v, "RMB"+v)
		if strings.HasSuffix(v, ".00") {
			out = append(out, strings.TrimSuffix(v, ".00"))
		}
		if f, err := parseAmount(v); err == nil {
			// 千分位写法：1,234.00
			out = append(out, thousand(f))
		}
	case judge.FIssueDate:
		// 2026-11-01 → 2026-11-01 / 2026/11/01 / 2026年11月1日 / 2026年11月01日
		parts := strings.Split(v, "-")
		if len(parts) == 3 {
			y, m, d := parts[0], strings.TrimLeft(parts[1], "0"), strings.TrimLeft(parts[2], "0")
			if m == "" {
				m = "0"
			}
			if d == "" {
				d = "0"
			}
			out = append(out,
				y+"/"+parts[1]+"/"+parts[2],
				y+"年"+m+"月"+d+"日",
				y+"年"+parts[1]+"月"+parts[2]+"日",
				y+"/"+m+"/"+d,
			)
		}
	}
	return out
}

// parseAmount 把 "1,234.00" 解析成 1234。
func parseAmount(s string) (float64, error) {
	s = strings.ReplaceAll(s, ",", "")
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
		return 0, err
	}
	return f, nil
}

// thousand 把 1234 写成 "1,234.00" 与 "1,234"。
func thousand(f float64) string {
	whole := int64(f*100+0.5) / 100
	s := fmt.Sprintf("%d", whole)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// squashSpaces 去掉全部空白，让"2026 年 11 月"与"2026年11月"能对上。
func squashSpaces(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// excerpt 截取命中位置前后的上下文。
func excerpt(text string, idx, n int) string {
	const pad = 12
	start := idx - pad
	if start < 0 {
		start = 0
	}
	end := idx + n + pad
	if end > len(text) {
		end = len(text)
	}
	// 按字节切可能切坏多字节字符，这里对齐到 rune 边界。
	for start > 0 && !utf8Start(text[start]) {
		start--
	}
	for end < len(text) && !utf8Start(text[end]) {
		end++
	}
	s := text[start:end]
	if start > 0 {
		s = "…" + s
	}
	if end < len(text) {
		s = s + "…"
	}
	return s
}

// utf8Start 判断某字节是否是一个字符的起始字节。
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// countUntraced 统计"原件里找不到"的有值字段数。
func countUntraced(ts []FieldTrace) int {
	n := 0
	for _, t := range ts {
		if t.Source == TraceNone {
			n++
		}
	}
	return n
}
