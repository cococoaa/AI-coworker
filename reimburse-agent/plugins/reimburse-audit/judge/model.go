package judge

// 本文件是判定引擎对"抽取结果"的最小视图。
//
// 早先引擎直接吃 internal/llm 的 Extraction / DualResult，等于把插件绑死在
// 某一个模型网关的实现上。fork 者换成别的模型栈就编译不过。
// 现在插件只认这里定义的结构，宿主负责把自己的抽取结果翻译过来（见 Extract 的说明）。
//
// 翻译是单向、显式的：字段少了编译不过，字段类型变了编译不过，不会悄悄丢数据。

import "context"

// FieldName 是可抽取的字段。用枚举而不是字符串，拼错就编译不过。
type FieldName string

const (
	FInvoiceNo   FieldName = "invoice_no"
	FIssueDate   FieldName = "issue_date"
	FTotalAmount FieldName = "total_amount"
	FBuyerTaxID  FieldName = "buyer_tax_id"
	FSellerTaxID FieldName = "seller_tax_id"
	FBuyerName   FieldName = "buyer_name"
	FSellerName  FieldName = "seller_name"
	FItemName    FieldName = "item_name"
)

// FieldNames 返回全部字段，顺序固定，便于界面与报告稳定输出。
func FieldNames() []FieldName {
	return []FieldName{FInvoiceNo, FIssueDate, FTotalAmount, FBuyerTaxID, FSellerTaxID,
		FBuyerName, FSellerName, FItemName}
}

// KeyFields 是"关键字段"：这些字段不可信时必须转人工。
// 与 policy.json 的 params.key_fields 对应，此处作为编译期兜底。
func KeyFields() []FieldName {
	return []FieldName{FInvoiceNo, FIssueDate, FTotalAmount, FBuyerTaxID, FSellerTaxID}
}

// Field 是一个抽取字段。Value 约定：
//   - 金额：以"元"为单位、两位小数的字符串，如 "553.00"
//   - 日期：YYYY-MM-DD
//   - 其余：原文照抄
type Field struct {
	Value string
	// Conf 是合并后的置信度：两侧模型取较小值。
	Conf float64
}

// Missing 表示没有读到值。
func (f Field) Missing() bool { return f.Value == "" }

// Fields 是 8 个字段的集合。
type Fields map[FieldName]Field

// LowConfidenceThreshold 是默认的置信度门槛，policy 里可以覆盖。
const LowConfidenceThreshold = 0.8

// Trusted 判断某字段能否作为判定依据：有值、置信度达标、且未被标为两模型不一致。
func (fs Fields) Trusted(name FieldName, untrusted map[FieldName]bool, threshold float64) bool {
	if untrusted[name] {
		return false
	}
	f := fs[name]
	return !f.Missing() && f.Conf >= threshold
}

// Extraction 是一个模型对一张单据的抽取结果（单侧）。
type Extraction struct {
	// Provider 只用于展示"这是哪一侧抽的"，插件不据此做任何判定。
	Provider string
	Model    string
	Fields   Fields
	// Usage 与 Latency 仅用于事件展示。
	InputTokens  int
	OutputTokens int
	LatencyMS    int64
	Attempts     int
}

// Issue 是抽取失败的原因。Kind 取值由宿主定义（如 no_key / timeout / bad_json），
// 插件只要求：非空即表示这一侧不可用。
type Issue struct {
	Kind   string
	Detail string
}

// Dual 是双模型抽取的合并结果：两个单侧结果 + 合并后的字段 + 是否一致。
type Dual struct {
	A, B           *Extraction
	IssueA, IssueB *Issue

	// Fields 是合并结果：两侧一致时取 A 的值、置信度取较小者；
	// 不一致的字段不出现在这里，而是进 Untrusted。
	Fields Fields
	// Untrusted 是两侧看到的值不同、因而完全不可信的字段。
	Untrusted map[FieldName]bool
	// LowConf 是置信度低于门槛的字段（不含 Untrusted）。
	LowConf []FieldName
	// Disagree 与 Untrusted 同义，保留给界面展示"哪几个字段不一致"。
	Disagree []FieldName
	// BothOK 表示两个模型都成功抽取。
	BothOK bool
	// Degraded 是"只有一侧可用"的说明，空表示两侧都正常。
	Degraded string
}

// Extractor 是插件对模型的全部要求：用指定供应商抽一张单据。
// 宿主可以用任何模型栈实现它——双模型、单模型、回放、甚至是一个固定返回的测试桩。
type Extractor interface {
	// Extract 用 provider 抽取一张单据。第二个返回值非 nil 表示这一侧不可用。
	Extract(ctx context.Context, provider string, src Source) (*Extraction, *Issue)
	// Providers 返回宿主本次启用的供应商名，顺序即展示顺序。
	Providers() []string
}

// Source 是要抽取的单据原文。
type Source struct {
	Category    string // transport / lodging / meal
	InvoiceText string
	SupportText string // 可能为空
}

// Merge 把两侧抽取结果合并成 Dual。这是插件自己的逻辑，不依赖宿主。
//
// 比对口径：只做格式归一化（金额补两位小数、税号转大写、日期去空格），
// 不消除数值差异——"553.00" 与 "553" 视为一致，"553.00" 与 "553.01" 视为不一致。
func Merge(a, b *Extraction, ia, ib *Issue, threshold float64) *Dual {
	d := &Dual{A: a, B: b, IssueA: ia, IssueB: ib, Fields: Fields{}, Untrusted: map[FieldName]bool{}}
	d.BothOK = a != nil && b != nil
	switch {
	case a != nil && b == nil:
		d.Degraded = "只有 " + a.Provider + " 抽取成功"
	case a == nil && b != nil:
		d.Degraded = "只有 " + b.Provider + " 抽取成功"
	}
	for _, name := range FieldNames() {
		switch {
		case a != nil && b != nil:
			va, vb := a.Fields[name], b.Fields[name]
			na, nb := normalize(name, va.Value), normalize(name, vb.Value)
			if na != nb {
				d.Untrusted[name] = true
				d.Disagree = append(d.Disagree, name)
				continue
			}
			conf := va.Conf
			if vb.Conf < conf {
				conf = vb.Conf
			}
			d.Fields[name] = Field{Value: na, Conf: conf}
			if conf < threshold {
				d.LowConf = append(d.LowConf, name)
			}
		case a != nil:
			d.Fields[name] = Field{Value: normalize(name, a.Fields[name].Value), Conf: a.Fields[name].Conf}
		case b != nil:
			d.Fields[name] = Field{Value: normalize(name, b.Fields[name].Value), Conf: b.Fields[name].Conf}
		}
	}
	return d
}
