// Package audit 是按规则表判定报销单的引擎。
//
// 与 mockgen.Label 的关系：两者实现同一套口径，但输入不同。
// Label 直接读结构化真值，用来给测试集打标准答案；
// Engine 只读"从文本抽取出来的字段"和"MCP 工具返回的结果"，模拟线上真实链路。
// 两者对同一张单据必须得出相同的命中项集合与结论，这是本项目的核心回归断言
// （见 engine_test.go 的 TestEngineMatchesLabelOnAllCases）。
package audit

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"reimburse/internal/llm"
	"reimburse/internal/schema"
	"reimburse/rules"
)

// ToolStatus 是一次工具调用的结果状态。
type ToolStatus string

const (
	ToolOK      ToolStatus = "ok"      // 成功
	ToolFailed  ToolStatus = "failed"  // 失败：报错、乱码、结果不可信
	ToolTimeout ToolStatus = "timeout" // 超时：状态未知，既不当失败也不当通过
	ToolSkipped ToolStatus = "skipped" // 未调用：前置条件缺失（如发票号看不清，无从查起）
)

// ToolResult 是一个工具返回的数据。Data 为 nil 表示没有可用结果。
type ToolResult struct {
	Status    ToolStatus          `json:"status"`
	Detail    string              `json:"detail,omitempty"`
	Invoices  []schema.RegInvoice `json:"invoices,omitempty"`
	Companies []schema.Company    `json:"companies,omitempty"`
	History   []schema.HistRecord `json:"history,omitempty"`
	// Raw 是工具返回的原始文本，用于界面展示与事后追溯。
	Raw string `json:"raw,omitempty"`
}

// Input 是引擎的全部输入。
type Input struct {
	Claim  *schema.Claim
	Fields map[string]llm.FieldValue // 抽取并双模型合并后的字段
	// Untrusted 是因为两个模型不一致而被判为不可信的字段。
	Untrusted []string
	// LowConfidence 是置信度低于阈值的字段。
	LowConfidence []string
	// SupportText 是佐证凭证原文，为空表示没有上传。佐证凭证的字段用确定性规则解析（见 support.go）。
	SupportText string
	Tools       map[string]ToolResult // 键为 verify_invoice / check_company / check_history

	supportDoc *SupportDoc
}

// Finding 是一条命中的判定项，带可追溯的依据。
type Finding struct {
	CheckID    string `json:"check_id"`
	RuleID     string `json:"rule_id"`
	RuleName   string `json:"rule_name"`
	Clause     string `json:"clause"`
	ClauseText string `json:"clause_text"`
	Outcome    string `json:"outcome"`
	When       string `json:"when"`
	// Reason 是用这张单据的实际数值写的一句话，说明为什么命中。
	Reason string `json:"reason"`
	// Skipped 记录因字段或工具不可用而未作判定的判定项及原因，便于人工复核时知道"没查什么"。
	Evidence []string `json:"evidence,omitempty"`
}

// Result 是判定结果。
type Result struct {
	Decision    string    `json:"decision"`
	Hits        []string  `json:"hits"`
	Findings    []Finding `json:"findings"`
	ApprovedFen int64     `json:"approved_fen"`
	// ClaimAmountFen 是申请金额，供审核意见核对金额时引用。
	ClaimAmountFen int64 `json:"claim_amount_fen"`
	// Skipped 是跳过的判定项：字段不可信或工具未给出可用结果。
	Skipped map[string]string `json:"skipped"`
	// Unverified 是因为工具失败/超时/未调用而没有核验的外部事实。
	Unverified []string `json:"unverified,omitempty"`
	// Verified 是已核验通过的外部事实，写进意见让审核员看到"查了什么、结果如何"。
	Verified []string `json:"verified,omitempty"`
	// Evidence 是判定过程中用到的原始数值，供意见核对金额时引用。
	Evidence []string `json:"evidence,omitempty"`
}

// Engine 持有规则表。
type Engine struct {
	RF *rules.File
}

func New(rf *rules.File) *Engine { return &Engine{RF: rf} }

// LowConfidenceNoCrossCheck 是编排层使用的标记：只有一个模型可用时，
// 关键字段没有经过交叉比对，按"未核验"处理。
const LowConfidenceNoCrossCheck = "cross_check"

// 解析后的工作视图。解析失败视为该字段不可信。
type view struct {
	claim    *schema.Claim
	p        *rules.Params
	inv      invoiceView
	amount   int64
	amountOK bool
	// 不可信字段
	bad map[string]bool
	// in 是原始输入，佐证凭证文本从中读取。
	in *Input
}

type invoiceView struct {
	InvoiceNo, IssueDate, BuyerTaxID, SellerTaxID, BuyerName, SellerName, ItemName string
	TotalFen                                                                       int64
}

// Judge 执行全部判定项。
func (e *Engine) Judge(in *Input) *Result {
	v := e.view(in)
	res := &Result{Skipped: map[string]string{}}
	hit := map[string]bool{}
	reason := map[string]string{}
	evid := map[string][]string{}

	add := func(id string, cond bool, why string, ev ...string) {
		if cond {
			hit[id] = true
			if reason[id] == "" {
				reason[id] = why
			}
			evid[id] = append(evid[id], ev...)
		}
	}
	// need 检查依赖的字段是否可信；不可信就记入 Skipped 并不作判定。
	need := func(id string, fields ...string) bool {
		for _, f := range fields {
			if v.bad[f] {
				if _, ok := res.Skipped[id]; !ok {
					res.Skipped[id] = "字段 " + fieldCN(f) + " 不可信（" + v.badWhy(f, in) + "）"
				}
				return false
			}
		}
		return true
	}
	// trusted 与 need 相同，但允许值为空：字段值本身可能真的是空（如个人抬头没有税号），
	// 只要读取过程可信、置信度够，就应当据此判定，而不是跳过。
	trusted := func(id string, fields ...string) bool {
		for _, f := range fields {
			if contains(in.Untrusted, f) {
				res.Skipped[id] = "字段 " + fieldCN(f) + " 两个模型抽取不一致"
				return false
			}
			fv, ok := in.Fields[f]
			if !ok || fv.Conf < v.p.LowConfidenceThreshold {
				res.Skipped[id] = "字段 " + fieldCN(f) + " 不可信（" + v.badWhy(f, in) + "）"
				return false
			}
		}
		return true
	}
	// tool 检查工具是否给出了可用结果。
	tool := func(t, id string) (ToolResult, bool) {
		tr, ok := in.Tools[t]
		if !ok || tr.Status != ToolOK {
			if _, dup := res.Skipped[id]; !dup {
				res.Skipped[id] = "工具 " + t + " " + toolCN(tr.Status)
				res.Unverified = appendUnique(res.Unverified, t)
			}
			return tr, false
		}
		return tr, true
	}
	// 本条规则不适用于该类别时记为不适用，而不是跳过。
	notApply := func(id, why string) { res.Skipped[id] = why }

	p := v.p
	cl := v.claim

	// R01 发票真实性与状态
	// R01 发票真实性与状态
	if !need("R01-a", llm.FInvoiceNo) {
		// 发票号不可信：无从查起，三个判定项都不作判定。
		res.Skipped["R01-b"] = res.Skipped["R01-a"]
		res.Skipped["R01-c"] = res.Skipped["R01-a"]
	} else if tr, ok := in.Tools["verify_invoice"]; ok && tr.Status == ToolOK {
		reg := findInvoice(tr, v.inv.InvoiceNo)
		if reg == nil {
			add("R01-b", true, "税务平台查无发票号码 "+v.inv.InvoiceNo, "verify_invoice.status=查无此票")
			res.Skipped["R01-a"] = "查无此票，不存在作废或红冲状态"
			res.Skipped["R01-c"] = "查无此票，无可比对的登记信息"
		} else {
			statusCN := map[string]string{schema.InvNormal: "正常", schema.InvVoided: "已作废", schema.InvRedReverse: "已红冲"}
			add("R01-a", reg.Status == schema.InvVoided || reg.Status == schema.InvRedReverse,
				"税务平台登记该发票状态为"+statusCN[reg.Status], "verify_invoice.status="+reg.Status)
			diff := ""
			// 只有抽取到的开票日期可信时才比对日期；否则不比，免得拿空值去比。
			if !v.bad[llm.FIssueDate] && reg.IssueDate != v.inv.IssueDate {
				diff += fmt.Sprintf("开票日期 抽取 %s / 登记 %s；", v.inv.IssueDate, reg.IssueDate)
			}
			if v.amountOK && reg.TotalFen != v.inv.TotalFen {
				diff += fmt.Sprintf("价税合计 抽取 %s / 登记 %s；", fen(v.inv.TotalFen), fen(reg.TotalFen))
			}
			if diff != "" {
				add("R01-c", reg.Status == schema.InvNormal, "查验登记信息与发票不一致，"+diff, "verify_invoice")
			} else {
				res.Skipped["R01-c"] = "已查，一致"
				res.Verified = append(res.Verified, "发票查验：税务平台登记与发票面额、开票日期一致")
			}
			if reg.Status == schema.InvNormal && reg.Status != "" {
				// 记录登记信息，供意见核对时引用。
				res.Evidence = append(res.Evidence, fmt.Sprintf("登记开票日期 %s，登记价税合计 %s", reg.IssueDate, fen(reg.TotalFen)))
			}
		}
	} else {
		st := ToolSkipped
		if ok {
			st = tr.Status
		}
		res.Skipped["R01-a"] = "工具 verify_invoice " + toolCN(st)
		res.Unverified = appendUnique(res.Unverified, "verify_invoice")
		res.Skipped["R01-b"] = res.Skipped["R01-a"]
		res.Skipped["R01-c"] = res.Skipped["R01-a"]
	}

	// R02 购买方抬头与税号
	if need("R02-a", llm.FBuyerName) {
		add("R02-a", v.inv.BuyerName != p.Company.Name,
			fmt.Sprintf("购买方名称为「%s」，应为「%s」", v.inv.BuyerName, p.Company.Name), "invoice.buyer_name")
	}
	if trusted("R02-b", llm.FBuyerTaxID) {
		switch {
		case v.inv.BuyerTaxID == "":
			add("R02-b", true, "购买方纳税人识别号缺失", "invoice.buyer_tax_id=空")
		case v.inv.BuyerTaxID != p.Company.TaxID:
			add("R02-b", true, fmt.Sprintf("购买方税号 %s 与公司税号 %s 不一致", v.inv.BuyerTaxID, p.Company.TaxID), "invoice.buyer_tax_id")
		case !rules.ValidTaxID(v.inv.BuyerTaxID):
			add("R02-b", true, "购买方税号 "+v.inv.BuyerTaxID+" 校验位不合法", "taxid.checksum")
		}
	}

	// R03 内容与类别
	if need("R03-a", llm.FItemName) {
		prefix := p.CategoryItemPrefix[cl.Category]
		add("R03-a", !strings.HasPrefix(v.inv.ItemName, prefix),
			fmt.Sprintf("发票项目「%s」不属于%s（应以 %s 开头）", v.inv.ItemName, categoryCN(cl.Category), prefix), "invoice.item_name")
	}
	text := v.inv.ItemName
	if kw, bad := p.ForbiddenHit(text + " " + in.SupportText); bad {
		add("R03-b", true, "发票项目或消费清单含禁止项目「"+kw+"」", "forbidden_keywords")
	}

	// R04 重复报销
	isTrain := strings.Contains(v.inv.ItemName, p.TrainTicketKeyword)
	if need("R04-a", llm.FInvoiceNo) {
		if tr, ok := tool("check_history", "R04-a"); ok {
			res.Skipped["R04-b"] = "同上（已查重）"
			sameNo := histFilter(tr, func(h schema.HistRecord) bool {
				return h.InvoiceNo == v.inv.InvoiceNo && (h.Status == schema.HistApproved || h.Status == schema.HistReviewing)
			})
			if len(tr.History) > 0 && len(sameNo) == 0 {
				res.Verified = append(res.Verified, "历史查重：历史库中无相同发票号码的通过或审核中记录")
			}
			if len(sameNo) > 0 {
				add("R04-a", true, fmt.Sprintf("历史库已有同一发票号码 %s 的记录（%s，状态 %s）",
					v.inv.InvoiceNo, sameNo[0].ClaimNo, histStatusCN(sameNo[0].Status)), "check_history.invoice_no")
			}
			if need("R04-b", llm.FIssueDate, llm.FSellerTaxID) && v.amountOK {
				like := histFilter(tr, func(h schema.HistRecord) bool {
					return h.InvoiceNo != v.inv.InvoiceNo && h.ApplicantID == cl.Applicant.EmployeeID &&
						h.SellerTaxID == v.inv.SellerTaxID && h.IssueDate == v.inv.IssueDate &&
						h.AmountFen == v.inv.TotalFen && h.Status != schema.HistRejected
				})
				if len(like) > 0 {
					add("R04-b", !isTrain, fmt.Sprintf("同人同销售方同日同额、发票号不同的历史记录 %s（%s），疑似换票重报",
						like[0].ClaimNo, fen(like[0].AmountFen)), "check_history")
				}
			}
		}
	}
	if isTrain {
		notApply("R04-b", "火车票往返同日同额属正常，不适用")
	}

	// R05 招待审批线与拆分
	line := p.ApprovalLineFen()
	entertain := cl.Category == rules.CatMeal && cl.ExternalCount > 0
	if !entertain {
		notApply("R05-a", "非业务招待")
		notApply("R05-b", "非业务招待")
	} else if v.amountOK {
		if cl.EntertainApprovalNo == "" {
			add("R05-a", v.inv.TotalFen >= line, fmt.Sprintf("业务招待价税合计 %s 达到审批线 %s，申请单无事前审批号",
				fen(v.inv.TotalFen), fen(line)), "claim.entertain_approval_no=空")
		} else {
			res.Skipped["R05-a"] = "已填事前审批号，达标无需命中"
		}
		if need("R05-b", llm.FIssueDate, llm.FSellerTaxID) {
			if tr, ok := tool("check_history", "R05-b"); ok {
				if v.inv.TotalFen < line {
					sum, allBelow, related := v.inv.TotalFen, true, []string{}
					for _, h := range tr.History {
						if h.ApplicantID != cl.Applicant.EmployeeID || h.SellerTaxID != v.inv.SellerTaxID ||
							h.Category != rules.CatMeal || h.Status == schema.HistRejected || h.InvoiceNo == v.inv.InvoiceNo {
							continue
						}
						if d := daysAbs(h.IssueDate, v.inv.IssueDate); d > p.SplitWindowDays {
							continue
						}
						sum += h.AmountFen
						related = append(related, fmt.Sprintf("%s（%s，%s 前）", h.ClaimNo, fen(h.AmountFen), h.IssueDate))
						if h.AmountFen >= line {
							allBelow = false
						}
					}
					add("R05-b", len(related) > 0 && allBelow && sum >= line,
						fmt.Sprintf("window 内同店餐饮合计 %s 达到审批线 %s，且各笔均低于审批线：%s",
							fen(sum), fen(line), strings.Join(related, "、")), "check_history.split")
				} else {
					res.Skipped["R05-b"] = "本单已达审批线，拆分无意义"
				}
			}
		}
	}

	// R06 金额三方一致
	if v.amountOK && need("R06-a", llm.FTotalAmount) {
		add("R06-a", cl.AmountFen > v.inv.TotalFen, fmt.Sprintf("申请金额 %s 高于发票价税合计 %s",
			fen(cl.AmountFen), fen(v.inv.TotalFen)), "claim.amount_fen")
	}
	supportTotal, hasSupportTotal := supportTotal(in)
	if hasSupportTotal && v.amountOK {
		add("R06-b", abs(v.inv.TotalFen-supportTotal) > p.AmountToleranceFen,
			fmt.Sprintf("发票价税合计 %s 与佐证凭证合计 %s 不一致", fen(v.inv.TotalFen), fen(supportTotal)), "support.total")
	} else if !hasSupportTotal {
		notApply("R06-b", "无佐证凭证合计可对")
	}

	// R07 时效
	add("R07-a", daysBetween(cl.EndDate, cl.SubmitDate) > p.ReimburseDeadlineDays,
		fmt.Sprintf("提交日 %s 距出差结束日 %s 已 %d 天，超过 %d 天", cl.SubmitDate, cl.EndDate,
			daysBetween(cl.EndDate, cl.SubmitDate), p.ReimburseDeadlineDays), "claim.submit_date")

	// R08 日期吻合
	switch cl.Category {
	case rules.CatTransport:
		if d, ok := supportDate(in, "date"); ok && need("R08-a", llm.FIssueDate) {
			lo := addDays(cl.StartDate, -p.TransportDateBufferDays)
			hi := addDays(cl.EndDate, p.TransportDateBufferDays)
			add("R08-a", d < lo || d > hi, fmt.Sprintf("乘车日 %s 不在申请期 %s~%s（含前后 %d 天缓冲）内",
				d, cl.StartDate, cl.EndDate, p.TransportDateBufferDays), "support.date")
		} else if !ok {
			notApply("R08-a", "无行程单日期")
		}
	case rules.CatLodging:
		inD, ok1 := supportDate(in, "check_in")
		outD, ok2 := supportDate(in, "check_out")
		if ok1 && ok2 {
			add("R08-a", inD < cl.StartDate || outD > cl.EndDate || inD > outD,
				fmt.Sprintf("入住 %s / 离店 %s 与申请期 %s~%s 不符", inD, outD, cl.StartDate, cl.EndDate), "support.check_in")
		} else {
			notApply("R08-a", "水单缺少入住或离店日期")
		}
	case rules.CatMeal:
		if d, ok := supportDate(in, "date"); ok {
			add("R08-a", d < cl.StartDate || d > cl.EndDate,
				fmt.Sprintf("用餐日 %s 不在申请期 %s~%s 内", d, cl.StartDate, cl.EndDate), "support.date")
		} else {
			notApply("R08-a", "无消费清单日期")
		}
	}

	// R09 席别
	if cl.Category == rules.CatTransport {
		seat, ok := supportField(in, "seat")
		if ok && seat != "" {
			if !p.SeatAllowed(supportMode(in), cl.Applicant.Grade, seat) {
				if cl.SpecialApprovalNo == "" {
					add("R09-a", true, fmt.Sprintf("%s席别「%s」超出 %s 职级允许范围，且无特批编号",
						modeCN(supportMode(in)), seat, cl.Applicant.Grade), "support.seat")
				} else {
					add("R09-b", true, fmt.Sprintf("%s席别「%s」超出职级范围，已填特批编号 %s（真伪待人工确认）",
						modeCN(supportMode(in)), seat, cl.SpecialApprovalNo), "claim.special_approval_no")
				}
			}
		} else {
			notApply("R09-a", "行程单缺少席别")
			notApply("R09-b", "行程单缺少席别")
		}
	} else {
		notApply("R09-a", "非交通类")
		notApply("R09-b", "非交通类")
	}

	// R10 住宿限额、R11 餐饮人均
	var caps []struct {
		id  string
		fen int64
		why string
	}
	if cl.Category == rules.CatLodging {
		notApply("R11-a", "非餐饮类")
		if n, ok := supportInt(in, "nights"); ok && n > 0 {
			if v.amountOK {
				capFen := p.LodgingLimitFen(cl.City, cl.Applicant.Grade) * int64(n)
				if v.inv.TotalFen > capFen {
					add("R10-a", true, fmt.Sprintf("%s 属%s，%s 职级夜均限额 %s，%d 晚合计 %s，实际 %s（夜均 %s）",
						cl.City, tierCN(p.CityTier(cl.City)), cl.Applicant.Grade, fen(p.LodgingLimitFen(cl.City, cl.Applicant.Grade)),
						n, fen(capFen), fen(v.inv.TotalFen), fen(v.inv.TotalFen/int64(n))), "rules.lodging_limit")
					caps = append(caps, struct {
						id  string
						fen int64
						why string
					}{"R10-a", capFen, "按夜均限额核准"})
				}
			}
		} else {
			notApply("R10-a", "水单缺少晚数")
		}
	} else {
		notApply("R10-a", "非住宿类")
	}
	if cl.Category == rules.CatMeal {
		n := int64(cl.InternalCount + cl.ExternalCount)
		if n > 0 && v.amountOK {
			capFen := p.MealCapFen(cl.ExternalCount > 0) * n
			if v.inv.TotalFen > capFen {
				add("R11-a", true, fmt.Sprintf("共 %d 人，%s人均上限 %s，合计上限 %s，实际 %s（人均 %s）",
					n, mealKindCN(cl.ExternalCount > 0), fen(p.MealCapFen(cl.ExternalCount > 0)), fen(capFen),
					fen(v.inv.TotalFen), fen(v.inv.TotalFen/n)), "rules.meal_cap")
				caps = append(caps, struct {
					id  string
					fen int64
					why string
				}{"R11-a", capFen, "按人均上限核准"})
			}
		} else if n == 0 {
			notApply("R11-a", "未填人数")
		}
	}

	// R12 本人
	if cl.Category == rules.CatTransport || cl.Category == rules.CatLodging {
		if who, ok := supportField(in, personKey(cl.Category)); ok && who != "" {
			add("R12-a", who != cl.Applicant.Name, fmt.Sprintf("%s「%s」与申请人「%s」不一致",
				personCN(cl.Category), who, cl.Applicant.Name), "support.person")
		} else {
			notApply("R12-a", "凭证缺少姓名")
		}
	} else {
		notApply("R12-a", "餐饮不核对本人")
	}

	// R13 销售方状态
	if need("R13-a", llm.FSellerTaxID) {
		if tr, ok := tool("check_company", "R13-a"); ok {
			res.Skipped["R13-b"] = "同上（已核验）"
			co := findCompany(tr, v.inv.SellerTaxID)
			if co == nil {
				// 企业库查无此税号：不是注销也不是经营异常，但属于无法确认，转人工。
				add("R13-b", true, "企业登记库查无销售方税号 "+v.inv.SellerTaxID+"，无法确认企业状态", "check_company.查无")
			} else {
				add("R13-a", co.Status == schema.CoCanceled || co.Status == schema.CoRevoked,
					fmt.Sprintf("销售方「%s」登记状态为%s", co.Name, co.Status), "check_company.status")
				add("R13-b", co.Status == schema.CoAbnormal,
					fmt.Sprintf("销售方「%s」登记状态为%s", co.Name, co.Status), "check_company.status")
				if co.Status == schema.CoActive {
					res.Verified = append(res.Verified, fmt.Sprintf("企业核验：销售方「%s」登记状态为存续", co.Name))
				}
			}
		}
	}

	// R14 凭证完整性
	_, hasSupport := supportRendered(in)
	add("R14-a", !hasSupport, "未上传对应的行程单/水单/消费清单，附件佐证凭证为空", "claim.attachments.support_file=空")
	missing := []string{}
	if cl.RequestNo == "" {
		missing = append(missing, "缺申请单号")
	}
	if cl.Category == rules.CatMeal {
		if cl.Purpose == "" {
			missing = append(missing, "缺事由")
		}
		if cl.InternalCount+cl.ExternalCount == 0 {
			missing = append(missing, "缺人数")
		}
		if cl.ExternalCount > 0 && cl.ExternalOrg == "" {
			missing = append(missing, "有外部人员但缺招待对象单位")
		}
	}
	add("R14-b", len(missing) > 0, "报销单信息不完整："+strings.Join(missing, "、"), "claim")

	// R15 核验失败与低置信度。本条自身不受跳过影响。
	var unverifiedWhy []string
	for _, t := range []string{"verify_invoice", "check_company", "check_history"} {
		tr, ok := in.Tools[t]
		if !ok {
			continue
		}
		switch tr.Status {
		case ToolFailed:
			unverifiedWhy = append(unverifiedWhy, t+" 调用失败（"+"结果不可用"+")")
		case ToolTimeout:
			unverifiedWhy = append(unverifiedWhy, t+" 超时（状态未知）")
		}
	}
	add("R15-a", len(unverifiedWhy) > 0, strings.Join(unverifiedWhy, "；"), "mcp.tool")
	var lowWhy []string
	// 单模型可用、没有交叉校验：即使字段置信度很高，也不能宣称已交叉验证。
	if contains(in.LowConfidence, LowConfidenceNoCrossCheck) {
		lowWhy = append(lowWhy, "另一模型不可用，关键字段缺少交叉校验")
	}
	for _, f := range llm.ExtractFields {
		fv, ok := in.Fields[f]
		if !ok {
			continue
		}
		if contains(in.Untrusted, f) {
			lowWhy = append(lowWhy, fieldCN(f)+" 两个模型抽取不一致")
		} else if fv.Conf < p.LowConfidenceThreshold {
			lowWhy = append(lowWhy, fmt.Sprintf("%s 置信度 %.2f 低于 %.2f", fieldCN(f), fv.Conf, p.LowConfidenceThreshold))
		}
	}
	add("R15-b", len(lowWhy) > 0, strings.Join(lowWhy, "；"), "llm.extraction")

	// 汇总：按规则表顺序输出命中项。
	for _, id := range e.RF.AllCheckIDs() {
		if hit[id] {
			res.Hits = append(res.Hits, id)
			r, c, _ := e.RF.FindCheck(id)
			res.Findings = append(res.Findings, Finding{
				CheckID: id, RuleID: r.ID, RuleName: r.Name, Clause: r.Clause, ClauseText: r.ClauseText,
				Outcome: c.Outcome, When: c.When, Reason: reason[id], Evidence: evid[id],
			})
		}
	}
	res.ClaimAmountFen = cl.AmountFen
	res.Decision = e.RF.Aggregate(res.Hits)
	switch res.Decision {
	case rules.Approve:
		res.ApprovedFen = cl.AmountFen
	case rules.ApproveAdj:
		res.ApprovedFen = cl.AmountFen
		for _, c := range caps {
			if c.fen < res.ApprovedFen {
				res.ApprovedFen = c.fen
			}
		}
	default:
		res.ApprovedFen = 0
	}
	sort.Strings(res.Unverified)
	return res
}

// ---------- 解析与工具函数 ----------

func (e *Engine) view(in *Input) *view {
	p := &e.RF.Params
	v := &view{claim: in.Claim, p: p, bad: map[string]bool{}, in: in}
	for _, f := range llm.ExtractFields {
		fv, ok := in.Fields[f]
		if !ok || fv.Value == "" || fv.Conf < p.LowConfidenceThreshold || contains(in.Untrusted, f) {
			v.bad[f] = true
			continue
		}
		switch f {
		case llm.FInvoiceNo:
			v.inv.InvoiceNo = strings.TrimSpace(fv.Value)
		case llm.FIssueDate:
			d, err := normDate(fv.Value)
			if err != nil {
				v.bad[f] = true
				continue
			}
			v.inv.IssueDate = d
		case llm.FTotalAmount:
			fenVal, err := parseYuan(fv.Value)
			if err != nil {
				v.bad[f] = true
				continue
			}
			v.inv.TotalFen, v.amountOK = fenVal, true
		case llm.FBuyerTaxID:
			v.inv.BuyerTaxID = fv.Value
		case llm.FSellerTaxID:
			v.inv.SellerTaxID = fv.Value
		case llm.FBuyerName:
			v.inv.BuyerName = fv.Value
		case llm.FSellerName:
			v.inv.SellerName = fv.Value
		case llm.FItemName:
			v.inv.ItemName = fv.Value
		}
	}
	if v.bad[llm.FInvoiceNo] {
		v.inv.InvoiceNo = ""
	}
	return v
}

func (v *view) badWhy(f string, in *Input) string {
	var ws []string
	if contains(in.Untrusted, f) {
		ws = append(ws, "两个模型不一致")
	}
	if fv, ok := in.Fields[f]; ok {
		if fv.Value == "" {
			ws = append(ws, "未能读取")
		} else if fv.Conf < v.p.LowConfidenceThreshold {
			ws = append(ws, fmt.Sprintf("置信度 %.2f", fv.Conf))
		}
	}
	if len(ws) == 0 {
		ws = append(ws, "无法解析")
	}
	return strings.Join(ws, "，")
}

func findInvoice(tr ToolResult, no string) *schema.RegInvoice {
	for i := range tr.Invoices {
		if tr.Invoices[i].InvoiceNo == no {
			return &tr.Invoices[i]
		}
	}
	return nil
}

func findCompany(tr ToolResult, taxID string) *schema.Company {
	for i := range tr.Companies {
		if tr.Companies[i].TaxID == taxID {
			return &tr.Companies[i]
		}
	}
	return nil
}

func histFilter(tr ToolResult, f func(schema.HistRecord) bool) []schema.HistRecord {
	var out []schema.HistRecord
	for _, h := range tr.History {
		if f(h) {
			out = append(out, h)
		}
	}
	return out
}

func normDate(s string) (string, error) {
	for _, layout := range []string{"2006-01-02", "2006/01/02", "2006年01月02日", "2006.01.02"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t.Format("2006-01-02"), nil
		}
	}
	// "2026-1-2" 这类不补零的写法。
	if t, err := time.Parse("2006-1-2", strings.TrimSpace(s)); err == nil {
		return t.Format("2006-01-02"), nil
	}
	return "", fmt.Errorf("日期格式无法识别: %q", s)
}

func parseYuan(s string) (int64, error) {
	t := strings.TrimSpace(s)
	t = strings.ReplaceAll(t, "¥", "")
	t = strings.ReplaceAll(t, "￥", "")
	t = strings.ReplaceAll(t, ",", "")
	t = strings.ReplaceAll(t, " ", "")
	if t == "" {
		return 0, fmt.Errorf("金额为空")
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, err
	}
	// 四舍五入到分，避免浮点误差。
	return int64(f*100 + 0.5), nil
}

func addDays(s string, n int) string {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return s
	}
	return t.AddDate(0, 0, n).Format("2006-01-02")
}

func daysBetween(a, b string) int {
	ta, err1 := time.Parse("2006-01-02", a)
	tb, err2 := time.Parse("2006-01-02", b)
	if err1 != nil || err2 != nil {
		return 0
	}
	return int(tb.Sub(ta).Hours() / 24)
}

func daysAbs(a, b string) int {
	d := daysBetween(a, b)
	if d < 0 {
		return -d
	}
	return d
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func fen(v int64) string {
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("¥%s%d.%02d", sign, v/100, v%100)
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

func appendUnique(xs []string, x string) []string {
	if contains(xs, x) {
		return xs
	}
	return append(xs, x)
}

func histStatusCN(s string) string {
	switch s {
	case schema.HistApproved:
		return "已通过"
	case schema.HistReviewing:
		return "审核中"
	case schema.HistRejected:
		return "已驳回"
	}
	return s
}

func fieldCN(f string) string {
	switch f {
	case llm.FInvoiceNo:
		return "发票号码"
	case llm.FIssueDate:
		return "开票日期"
	case llm.FTotalAmount:
		return "价税合计"
	case llm.FBuyerTaxID:
		return "购买方税号"
	case llm.FSellerTaxID:
		return "销售方税号"
	case llm.FBuyerName:
		return "购买方名称"
	case llm.FSellerName:
		return "销售方名称"
	case llm.FItemName:
		return "发票项目"
	}
	return f
}

func toolCN(s ToolStatus) string {
	switch s {
	case ToolOK:
		return "成功"
	case ToolFailed:
		return "失败"
	case ToolTimeout:
		return "超时"
	case ToolSkipped:
		return "未调用"
	}
	return string(s)
}

func categoryCN(c string) string {
	switch c {
	case rules.CatTransport:
		return "交通"
	case rules.CatLodging:
		return "住宿"
	case rules.CatMeal:
		return "餐饮"
	}
	return c
}

func tierCN(t string) string {
	switch t {
	case "tier1":
		return "一线城市"
	case "tier2":
		return "二线城市"
	}
	return "其他城市"
}

func mealKindCN(ext bool) string {
	if ext {
		return "业务招待"
	}
	return "工作餐"
}

func modeCN(m string) string {
	if m == "flight" {
		return "飞机"
	}
	return "火车"
}

func personKey(cat string) string {
	if cat == rules.CatLodging {
		return "person"
	}
	return "person"
}

func personCN(cat string) string {
	if cat == rules.CatLodging {
		return "入住人"
	}
	return "乘车人"
}
