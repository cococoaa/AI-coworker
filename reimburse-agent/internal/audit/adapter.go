// Package audit 是宿主侧的编排层：把插件（判定）、模型层、MCP 工具层串成一次审核。
//
// 与插件的分工：
//
//	plugins/reimburse-audit/policy  制度数据（业务改）
//	plugins/reimburse-audit/judge   判定逻辑（研发改，零依赖）
//	internal/audit（本包）           编排：调用模型、调用工具、推进五阶段、产出事件流
//
// 本包承担两件插件不该管的事：
//  1. 与具体模型厂商打交道（internal/llm）；
//  2. 与具体工具协议打交道（internal/mcptools → Explorer 接口）。
//
// 它把两边翻译成插件要求的类型（judge.Input / judge.ToolOutcome），然后把判定交给 judge。
// 这样换模型、换工具协议都不用动插件。
package audit

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"reimburse/internal/llm"
	"reimburse/internal/schema"
	"reimburse/plugins/reimburse-audit/judge"
	"reimburse/plugins/reimburse-audit/policy"
)

// 把插件的类型别名出来，宿主与测试不用到处写 judge.xxx。
type (
	Engine       = judge.Engine
	Input        = judge.Input
	Result       = judge.Result
	Finding      = judge.Finding
	ToolOutcome  = judge.ToolOutcome
	Status       = judge.Status
	HistoryQuery = judge.HistoryQuery
	Fields       = judge.Fields
	Field        = judge.Field
	FieldName    = judge.FieldName
	Dual         = judge.Dual
	Extraction   = judge.Extraction
	Issue        = judge.Issue
)

// 状态常量别名。
const (
	Found       = judge.Found
	NotFound    = judge.NotFound
	Failed      = judge.Failed
	Timeout     = judge.Timeout
	ToolOK      = judge.Found
	ToolFailed  = judge.Failed
	ToolTimeout = judge.Timeout
)

// 字段名别名。
const (
	FInvoiceNo   = judge.FInvoiceNo
	FIssueDate   = judge.FIssueDate
	FTotalAmount = judge.FTotalAmount
	FBuyerTaxID  = judge.FBuyerTaxID
	FSellerTaxID = judge.FSellerTaxID
	FBuyerName   = judge.FBuyerName
	FSellerName  = judge.FSellerName
	FItemName    = judge.FItemName
)

// LowConfidenceNoCrossCheck 见 judge 包。
const LowConfidenceNoCrossCheck = judge.LowConfidenceNoCrossCheck

// New 创建判定引擎（实际实现在插件里）。
func New(rf *policy.File) *Engine { return judge.New(rf) }

// ExtractFields 返回全部字段名，供宿主与测试使用。
func ExtractFields() []judge.FieldName { return judge.FieldNames() }

// Explorer 是 MCP 工具层在宿主侧的实现（见 internal/mcptools）。
// 它返回宿主自己的 ToolOutcome（含原始返回），编排层负责翻译成判定插件要的形式。
type Explorer interface {
	VerifyInvoice(ctx context.Context, invoiceNo string) ToolOutcome
	CheckCompany(ctx context.Context, taxID string) ToolOutcome
	CheckHistory(ctx context.Context, q HistoryQuery) ToolOutcome
}

// LLM 是编排层对模型层的最小依赖。
type LLM interface {
	ExtractWith(ctx context.Context, provider string, src llm.Source, onCall func(*llm.Response)) (*llm.Extraction, *llm.SchemaIssue)
}

// toJudgeTools 把宿主的工具结果翻译成插件要求的四态 + 记录。
func toJudgeTools(tools map[string]ToolOutcome) map[string]judge.ToolOutcome {
	out := make(map[string]judge.ToolOutcome, len(tools))
	for k, v := range tools {
		out[k] = v
	}
	return out
}

// toJudgeFields 把宿主的抽取结果翻译成插件字段集合。
func toJudgeFields(d *llm.DualResult) (judge.Fields, map[judge.FieldName]bool, []judge.FieldName) {
	fs := judge.Fields{}
	un := map[judge.FieldName]bool{}
	for _, name := range judge.FieldNames() {
		fv, ok := d.Fields[string(name)]
		if !ok {
			continue
		}
		fs[name] = judge.Field{Value: fv.Value, Conf: fv.Conf}
		if containsStr(d.Untrusted, string(name)) {
			un[name] = true
		}
	}
	low := make([]judge.FieldName, 0, len(d.LowConf))
	for _, f := range d.LowConf {
		low = append(low, judge.FieldName(f))
	}
	return fs, un, low
}

// toJudgeClaim 把 schema.Claim 翻译成判定用的最小视图。
func toJudgeClaim(c *schema.Claim) *judge.Claim {
	return &judge.Claim{
		ClaimNo: c.ClaimNo, RequestNo: c.RequestNo,
		Applicant: judge.Applicant{Name: c.Applicant.Name, EmployeeID: c.Applicant.EmployeeID, Grade: c.Applicant.Grade},
		Category:  c.Category, FromCity: c.FromCity, City: c.City,
		StartDate: c.StartDate, EndDate: c.EndDate, Purpose: c.Purpose,
		AmountFen: c.AmountFen, SubmitDate: c.SubmitDate,
		InternalCount: c.InternalCount, ExternalCount: c.ExternalCount, ExternalOrg: c.ExternalOrg,
		EntertainApprovalNo: c.EntertainApprovalNo, SpecialApprovalNo: c.SpecialApprovalNo,
		SupportFile: c.Attachments.SupportFile,
	}
}

var _ = fmt.Sprintf
var _ = sort.Strings
var _ = strings.TrimSpace
var _ = atomic.AddInt64
var _ = time.Now
