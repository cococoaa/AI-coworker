package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"reimburse/internal/schema"
)

// Synthetic 在没有密钥、也没有真实录制时，用"标准答案 + 人为扰动"合成一份模型输出。
//
// 用途只有一个：让演示在没有密钥的环境下也能看到完整的抽取→比对→判定链路。
// 因此有三条硬约束：
//  1. 必须在界面上明确标注"合成"，并记进报告与事件流，绝不能冒充真实模型输出；
//  2. 扰动是确定性的（按用例编号与供应商派生），同一条用例每次结果相同，便于复现；
//  3. 合成结果仍要过与真实响应完全相同的 Schema 校验与交叉比对逻辑——
//     这是为了让"两模型不一致""置信度不足"这两条降级路径也能被演示到。
type Synthetic struct {
	// 让 Claude 的某一类字段产生偏差的比例，用来说明"交叉比对能抓出差异"。
	DeviationRate float64
}

// Boom 表示该供应商在本项目里被认为不可用。
func (s *Synthetic) Build(c *schema.Case, provider string) *Response {
	inv := c.Truth.Invoice
	un := map[string]bool{}
	for _, f := range c.Truth.Unreadable {
		un[f] = true
	}
	val := func(name, v string) map[string]any {
		conf := 1.0
		if un[name] {
			// 污损字段：值留空、置信度很低，模拟"看不清"而不是"看见是空的"。
			return map[string]any{"value": "", "confidence": 0.2}
		}
		if name == FBuyerTaxID && c.Truth.Invoice.BuyerTaxID == "" {
			conf = 1
		}
		return map[string]any{"value": v, "confidence": conf}
	}
	fields := map[string]any{
		FInvoiceNo:   val(FInvoiceNo, inv.InvoiceNo),
		FIssueDate:   val(FIssueDate, inv.IssueDate),
		FTotalAmount: val(FTotalAmount, fmt.Sprintf("%d.%02d", inv.TotalFen/100, inv.TotalFen%100)),
		FBuyerTaxID:  val(FBuyerTaxID, inv.BuyerTaxID),
		FSellerTaxID: val(FSellerTaxID, inv.SellerTaxID),
		FBuyerName:   val(FBuyerName, inv.BuyerName),
		FSellerName:  val(FSellerName, inv.SellerName),
		FItemName:    val(FItemName, inv.ItemName),
	}
	notes := ""
	// 这里刻意不加任何噪声。
	//
	// 曾经试过"让一侧抄错金额"或"让一侧置信度偏低"来演示降级，两次都失败了：
	//   - 抄错金额：凭空制造"两个模型不一致"，把本来正确的判定项淹没，评测里 15 条用例误报 R15-b；
	//   - 单侧低置信度：合并置信度取两侧较小值，同样让整单转人工，结论虽然对，但命中项全错。
	// 根本原因是——合成数据的职责是"在无密钥时复现判定链路"，不是"模拟模型的不确定性"。
	// 降级路径由彩排（cmd/rehearse）用显式开关演示，那里能精确控制关的是哪个模型、注入什么故障。
	//
	// 因此：合成输出 = 标准答案，两个供应商完全一致，置信度 1.0；
	// 只有用例本身就标了"字段污损"的，才输出空值 + 低置信度。
	obj := map[string]any{"fields": fields, "notes": notes}
	b, _ := json.Marshal(obj)
	return &Response{Provider: provider, Model: "synthetic", Text: string(b),
		Usage: Usage{InputTokens: 1200, OutputTokens: 260}, LatencyMS: 380, Attempts: 1}
}

// ParseMoneyFen 把 "553.00" 解析为分。合成与测试共用，避免两处实现出现分歧。
func ParseMoneyFen(s string) (int64, error) {
	var whole, frac int64
	if _, err := fmt.Sscanf(s, "%d.%d", &whole, &frac); err != nil {
		return 0, err
	}
	return whole*100 + frac, nil
}

// SyntheticExtract 实现与真实抽取相同的解析路径：先出文本，再过 parseExtraction。
// 这样合成数据也必须满足 Schema，不会绕开校验。
func (s *Synthetic) SyntheticExtract(ctx context.Context, provider string, c *schema.Case) (*Extraction, *SchemaIssue) {
	resp := s.Build(c, provider)
	ex, issue := parseExtraction(resp.Text)
	if issue != nil {
		return nil, issue
	}
	ex.Provider, ex.Model = provider, "synthetic"
	ex.Usage, ex.LatencyMS, ex.Attempts = resp.Usage, resp.LatencyMS, 1
	return ex, nil
}

// Extractor 与编排层的 LLM 接口签名一致（由 audit.LLM 定义，这里不复用避免反向依赖）。
type Extractor interface {
	ExtractWith(ctx context.Context, provider string, src Source, onCall func(*Response)) (*Extraction, *SchemaIssue)
}

// SyntheticLLM 让合成抽取接入编排层，签名与真实 LLM 一致。
// Case 由调用方在构造时绑定：一次审核只服务一张单据。
type SyntheticLLM struct {
	Case     *schema.Case
	Synth    *Synthetic
	Fallback Extractor // 可选：有密钥时优先真实调用
}

// ExtractWith 优先真实调用，失败才用合成。
func (s *SyntheticLLM) ExtractWith(ctx context.Context, provider string, src Source, onCall func(*Response)) (*Extraction, *SchemaIssue) {
	if s.Fallback != nil {
		if ex, issue := s.Fallback.ExtractWith(ctx, provider, src, onCall); issue == nil {
			return ex, nil
		}
	}
	if s.Case == nil {
		return nil, &SchemaIssue{Kind: ErrUnsupported, Detail: "没有绑定用例，无法合成"}
	}
	if !s.Case.HasTruth {
		// 合成抽取的原理就是从标准答案回放字段。没有标准答案，合成只能编，
		// 而编出来的字段会进判定、进查重——那比"跑不了"危险得多。
		// 这里明确拒绝，由调用方给出"该单据没有标准答案，合成模式不可用"的提示。
		return nil, &SchemaIssue{Kind: ErrUnsupported,
			Detail: "该单据没有标准答案（truth.json），合成演示模式无法用于真实票据"}
	}
	return s.Synth.SyntheticExtract(ctx, provider, s.Case)
}

// WriteSyntheticTape 把合成结果落成录制文件，格式与真实录制完全一致。
// 只在明确需要"预置回放数据"时调用，落盘的文件必须由调用方标注为合成来源。
func WriteSyntheticTape(dir, provider string, c *schema.Case) error {
	resp := (&Synthetic{}).Build(c, provider)
	p := TapePath(dir, provider, c.ID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	t := Tape{Provider: provider, Model: "synthetic（合成，非真实模型输出）", CaseID: c.ID,
		PromptV: tapeVersion, Text: resp.Text, Usage: resp.Usage, LatencyMS: resp.LatencyMS,
		RecordedAt: "合成：由标准答案生成，非模型真实输出"}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}
