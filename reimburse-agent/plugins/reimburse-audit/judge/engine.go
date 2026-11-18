// 引擎主体：按 policy 逐条判定。
//
// 与 mockgen.Label 的关系：两者实现同一套口径，但输入不同。
// Label 直接读结构化真值，用来给测试集打标准答案；
// Engine 只读"从文本抽取出来的字段"和"MCP 工具返回的结果"，模拟线上真实链路。
// 两者对同一张单据必须得出相同的命中项集合与结论，这是本项目的核心回归断言
// （见 conformance 工具）。
package judge

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"reimburse/plugins/reimburse-audit/policy"
)

// Engine 持有规则表。
type Engine struct {
	RF *policy.File
}

func New(rf *policy.File) *Engine { return &Engine{RF: rf} }

// LowConfidenceNoCrossCheck 是编排层使用的标记：只有一个模型可用时，
// 关键字段没有经过交叉比对，按"未核验"处理。
const LowConfidenceNoCrossCheck = "cross_check"

// 解析后的工作视图。解析失败视为该字段不可信。
type view struct {
	claim    *Claim
	p        *policy.Params
	inv      invoiceView
	amount   int64
	amountOK bool
	// 不可信字段（不可用作出判定依据）
	bad map[FieldName]bool
	// statuses 是每个字段的四态，区分"原件没有"与"看不清"
	statuses Statuses
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
	// need 检查依赖的字段能否作为判定依据。
	//
	// 关键变化：Absent（原件确实没填）**可以用**——"这张票没有税号"是可判定的业务事实；
	// 只有 Unreadable / Ambiguous 才跳过。早先一律按"值为空"跳过，
	// 导致 C24 那种"确实没税号"被误当成"读不出税号"，依据写成 R15 而不是 R02-b。
	need := func(id string, fields ...FieldName) bool {
		for _, f := range fields {
			if v.statuses.Of(f).Usable() {
				continue
			}
			if _, ok := res.Skipped[id]; !ok {
				res.Skipped[id] = "字段 " + StatusLabel(f, v.statuses.Of(f))
			}
			return false
		}
		return true
	}
	// trusted 与 need 同义，保留名字便于阅读；现在 Absent 也算可信。
	trusted := need
	// tool 检查工具是否给出了可用结果。
	tool := func(t, id string) (ToolOutcome, bool) {
		tr, ok := in.Tools[t]
		if !ok || tr.Status.Status != Found {
			if _, dup := res.Skipped[id]; !dup {
				res.Skipped[id] = "工具 " + t + " " + toolCN(tr.Status.Status)
				res.Unverified = appendStr(res.Unverified, t)
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
	if !need("R01-a", FInvoiceNo) {
		// 发票号不可信：无从查起，三个判定项都不作判定。
		res.Skipped["R01-b"] = res.Skipped["R01-a"]
		res.Skipped["R01-c"] = res.Skipped["R01-a"]
	} else if tr, ok := in.Tools["verify_invoice"]; ok && tr.Status.Status.Usable() {
		// 四态里 Usable 的两种：Found 与 NotFound。
		// NotFound 是"查无此票"这个确定事实，必须据此命中 R01-b；
		// 早先只在 Found 时进这个分支，导致"查无此票"被漏判成通过——
		// 这是把工具状态从字符串改成四态时引入的回归，conformance 抓到了。
		reg := findInvoice(tr, v.inv.InvoiceNo)
		if tr.Status.Status == NotFound || reg == nil {
			add("R01-b", true, "税务平台查无发票号码 "+v.inv.InvoiceNo, "verify_invoice.status=查无此票")
			res.Skipped["R01-a"] = "查无此票，不存在作废或红冲状态"
			res.Skipped["R01-c"] = "查无此票，无可比对的登记信息"
		} else {
			statusCN := map[string]string{invNormal: "正常", invVoided: "已作废", invRedReverse: "已红冲"}
			add("R01-a", reg.PlatformStatus == invVoided || reg.PlatformStatus == invRedReverse,
				"税务平台登记该发票状态为"+statusCN[reg.PlatformStatus], "verify_invoice.status="+reg.PlatformStatus)
			diff := ""
			// 只有抽取到的开票日期可信时才比对日期；否则不比，免得拿空值去比。
			if !v.bad[FIssueDate] && reg.IssueDate != v.inv.IssueDate {
				diff += fmt.Sprintf("开票日期 抽取 %s / 登记 %s；", v.inv.IssueDate, reg.IssueDate)
			}
			if v.amountOK && reg.TotalFen != v.inv.TotalFen {
				diff += fmt.Sprintf("价税合计 抽取 %s / 登记 %s；", fen(v.inv.TotalFen), fen(reg.TotalFen))
			}
			if diff != "" {
				add("R01-c", reg.PlatformStatus == invNormal, "查验登记信息与发票不一致，"+diff, "verify_invoice")
			} else {
				res.Skipped["R01-c"] = "已查，一致"
				res.Verified = append(res.Verified, "发票查验：税务平台登记与发票面额、开票日期一致")
			}
			if reg.PlatformStatus == invNormal && reg.PlatformStatus != "" {
				// 记录登记信息，供意见核对时引用。
				res.Evidence = append(res.Evidence, fmt.Sprintf("登记开票日期 %s，登记价税合计 %s", reg.IssueDate, fen(reg.TotalFen)))
			}
		}
	} else {
		st := Failed
		if ok {
			st = tr.Status.Status
		}
		res.Skipped["R01-a"] = "工具 verify_invoice " + toolCN(st)
		res.Unverified = appendStr(res.Unverified, "verify_invoice")
		res.Skipped["R01-b"] = res.Skipped["R01-a"]
		res.Skipped["R01-c"] = res.Skipped["R01-a"]
	}

	// R02 购买方抬头与税号
	if need("R02-a", FBuyerName) {
		add("R02-a", v.inv.BuyerName != p.Company.Name,
			fmt.Sprintf("购买方名称为「%s」，应为「%s」", v.inv.BuyerName, p.Company.Name), "invoice.buyer_name")
	}
	if trusted("R02-b", FBuyerTaxID) {
		switch {
		case v.inv.BuyerTaxID == "":
			add("R02-b", true, "购买方纳税人识别号缺失", "invoice.buyer_tax_id=空")
		case v.inv.BuyerTaxID != p.Company.TaxID:
			add("R02-b", true, fmt.Sprintf("购买方税号 %s 与公司税号 %s 不一致", v.inv.BuyerTaxID, p.Company.TaxID), "invoice.buyer_tax_id")
		case !policy.ValidTaxID(v.inv.BuyerTaxID):
			add("R02-b", true, "购买方税号 "+v.inv.BuyerTaxID+" 校验位不合法", "taxid.checksum")
		}
	}

	// R03 内容与类别
	if need("R03-a", FItemName) {
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
	if need("R04-a", FInvoiceNo) {
		if tr, ok := tool("check_history", "R04-a"); ok {
			res.Skipped["R04-b"] = "同上（已查重）"
			sameNo := histFilter(tr, func(h HistoryRecord) bool {
				return h.InvoiceNo == v.inv.InvoiceNo && (h.RecStatus == histApproved || h.RecStatus == histReviewing)
			})
			if len(tr.History) > 0 && len(sameNo) == 0 {
				res.Verified = append(res.Verified, "历史查重：历史库中无相同发票号码的通过或审核中记录")
			}
			if len(sameNo) > 0 {
				add("R04-a", true, fmt.Sprintf("历史库已有同一发票号码 %s 的记录（%s，状态 %s）",
					v.inv.InvoiceNo, sameNo[0].ClaimNo, histStatusCN(sameNo[0].RecStatus)), "check_history.invoice_no")
			}
			if need("R04-b", FIssueDate, FSellerTaxID) && v.amountOK {
				like := histFilter(tr, func(h HistoryRecord) bool {
					return h.InvoiceNo != v.inv.InvoiceNo && h.ApplicantID == cl.Applicant.EmployeeID &&
						h.SellerTaxID == v.inv.SellerTaxID && h.IssueDate == v.inv.IssueDate &&
						h.AmountFen == v.inv.TotalFen && h.RecStatus != histRejected
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
	entertain := cl.Category == policy.CatMeal && cl.ExternalCount > 0
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
		if need("R05-b", FIssueDate, FSellerTaxID) {
			if tr, ok := tool("check_history", "R05-b"); ok {
				if v.inv.TotalFen < line {
					sum, allBelow, related := v.inv.TotalFen, true, []string{}
					for _, h := range tr.History {
						if h.ApplicantID != cl.Applicant.EmployeeID || h.SellerTaxID != v.inv.SellerTaxID ||
							h.Category != policy.CatMeal || h.RecStatus == histRejected || h.InvoiceNo == v.inv.InvoiceNo {
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
	if v.amountOK && need("R06-a", FTotalAmount) {
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
	case policy.CatTransport:
		if d, ok := supportDate(in, "date"); ok && need("R08-a", FIssueDate) {
			lo := addDays(cl.StartDate, -p.TransportDateBufferDays)
			hi := addDays(cl.EndDate, p.TransportDateBufferDays)
			add("R08-a", d < lo || d > hi, fmt.Sprintf("乘车日 %s 不在申请期 %s~%s（含前后 %d 天缓冲）内",
				d, cl.StartDate, cl.EndDate, p.TransportDateBufferDays), "support.date")
		} else if !ok {
			notApply("R08-a", "无行程单日期")
		}
	case policy.CatLodging:
		inD, ok1 := supportDate(in, "check_in")
		outD, ok2 := supportDate(in, "check_out")
		if ok1 && ok2 {
			add("R08-a", inD < cl.StartDate || outD > cl.EndDate || inD > outD,
				fmt.Sprintf("入住 %s / 离店 %s 与申请期 %s~%s 不符", inD, outD, cl.StartDate, cl.EndDate), "support.check_in")
		} else {
			notApply("R08-a", "水单缺少入住或离店日期")
		}
	case policy.CatMeal:
		if d, ok := supportDate(in, "date"); ok {
			add("R08-a", d < cl.StartDate || d > cl.EndDate,
				fmt.Sprintf("用餐日 %s 不在申请期 %s~%s 内", d, cl.StartDate, cl.EndDate), "support.date")
		} else {
			notApply("R08-a", "无消费清单日期")
		}
	}

	// R09 席别
	if cl.Category == policy.CatTransport {
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
	if cl.Category == policy.CatLodging {
		notApply("R11-a", "非餐饮类")
		if n, ok := supportInt(in, "nights"); ok && n > 0 {
			if v.amountOK {
				capFen := p.LodgingLimitFen(cl.City, cl.Applicant.Grade) * int64(n)
				if v.inv.TotalFen > capFen {
					add("R10-a", true, fmt.Sprintf("%s 属%s，%s 职级夜均限额 %s，%d 晚合计 %s，实际 %s（夜均 %s）",
						cl.City, tierCN(p.CityTier(cl.City)), cl.Applicant.Grade, fen(p.LodgingLimitFen(cl.City, cl.Applicant.Grade)),
						n, fen(capFen), fen(v.inv.TotalFen), fen(v.inv.TotalFen/int64(n))), "policy.lodging_limit")
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
	if cl.Category == policy.CatMeal {
		n := int64(cl.InternalCount + cl.ExternalCount)
		if n > 0 && v.amountOK {
			capFen := p.MealCapFen(cl.ExternalCount > 0) * n
			if v.inv.TotalFen > capFen {
				add("R11-a", true, fmt.Sprintf("共 %d 人，%s人均上限 %s，合计上限 %s，实际 %s（人均 %s）",
					n, mealKindCN(cl.ExternalCount > 0), fen(p.MealCapFen(cl.ExternalCount > 0)), fen(capFen),
					fen(v.inv.TotalFen), fen(v.inv.TotalFen/n)), "policy.meal_cap")
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
	if cl.Category == policy.CatTransport || cl.Category == policy.CatLodging {
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
	if need("R13-a", FSellerTaxID) {
		if tr, ok := tool("check_company", "R13-a"); ok {
			res.Skipped["R13-b"] = "同上（已核验）"
			co := findCompany(tr, v.inv.SellerTaxID)
			if co == nil {
				// 企业库查无此税号：不是注销也不是经营异常，但属于无法确认，转人工。
				add("R13-b", true, "企业登记库查无销售方税号 "+v.inv.SellerTaxID+"，无法确认企业状态", "check_company.查无")
			} else {
				add("R13-a", co.RegStatus == coCanceled || co.RegStatus == coRevoked,
					fmt.Sprintf("销售方「%s」登记状态为%s", co.Name, co.RegStatus), "check_company.status")
				add("R13-b", co.RegStatus == coAbnormal,
					fmt.Sprintf("销售方「%s」登记状态为%s", co.Name, co.RegStatus), "check_company.status")
				if co.RegStatus == coActive {
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
	if cl.Category == policy.CatMeal {
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
		switch tr.Status.Status {
		case Failed:
			unverifiedWhy = append(unverifiedWhy, t+" 调用失败（"+"结果不可用"+")")
		case Timeout:
			unverifiedWhy = append(unverifiedWhy, t+" 超时（状态未知）")
		}
	}
	add("R15-a", len(unverifiedWhy) > 0, strings.Join(unverifiedWhy, "；"), "mcp.tool")
	var lowWhy []string
	// 单模型可用、没有交叉校验：即使字段置信度很高，也不能宣称已交叉验证。
	if containsField(in.LowConfidence, LowConfidenceNoCrossCheck) {
		lowWhy = append(lowWhy, "另一模型不可用，关键字段缺少交叉校验")
	}
	for _, f := range FieldNames() {
		fv, ok := in.Fields[f]
		if !ok {
			continue
		}
		st := v.statuses.Of(f)
		switch {
		case st == Absent:
			// 原件确实没填：这是业务事实，不构成"核验失败"。
			// 它会由依赖它的具体判定项（如 R02-b 税号缺失）处理，而不是在这里兜底转人工。
		case st == Ambiguous:
			lowWhy = append(lowWhy, FieldCN(f)+" 两个模型抽取不一致")
		case st == Unreadable:
			lowWhy = append(lowWhy, fmt.Sprintf("%s 无法识别（置信度 %.2f 低于 %.2f）",
				FieldCN(f), fv.Conf, p.LowConfidenceThreshold))
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
	res.FieldStatus = v.statuses
	res.Decision = e.RF.Aggregate(res.Hits)
	if strings.TrimSpace(cl.LateExplanation) != "" {
		var otherHits []string
		explained := false
		for i := range res.Findings {
			f := &res.Findings[i]
			if f.CheckID == "R07-a" && f.Outcome == policy.NeedMoreInfo {
				f.Outcome = policy.ManualReview
				f.Reason += "，已补交超期说明，待财务确认"
				explained = true
			} else {
				otherHits = append(otherHits, f.CheckID)
			}
		}
		if explained {
			res.Decision = e.RF.Aggregate(otherHits)
			if res.Decision == policy.Approve || res.Decision == policy.ApproveAdj {
				res.Decision = policy.ManualReview
			}
		}
	}
	switch res.Decision {
	case policy.Approve:
		res.ApprovedFen = cl.AmountFen
	case policy.ApproveAdj:
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
	v := &view{claim: in.Claim, p: p, bad: map[FieldName]bool{},
		statuses: Statuses{}, in: in}
	for _, f := range FieldNames() {
		fv, ok := in.Fields[f]
		// 由合并结果反推单侧信息：Fields 里是"两侧一致后的值"，
		// 因此这里能可靠判断的只有"有没有值 / 置信度 / 是否被标为不一致"。
		// 两侧都空且有高置信度 → 分类器判为 Absent（原件确实没有）。
		// 优先用模型显式给出的 presence；没有时才退回按置信度推断。
		st, decided := ClassifyFieldByPresence(f, fv, in.Untrusted[f], p.LowConfidenceThreshold)
		if !decided {
			st = ClassifyField(f, fv, fv, ok, ok, in.Untrusted[f], p.LowConfidenceThreshold)
		}
		v.statuses[f] = st
		if !st.Usable() {
			v.bad[f] = true
			continue
		}
		// Absent 没有值，不需要往下解析；它本身作为"缺失的事实"参与判定。
		if st == Absent {
			continue
		}
		switch f {
		case FInvoiceNo:
			v.inv.InvoiceNo = strings.TrimSpace(fv.Value)
		case FIssueDate:
			d, err := normDate(fv.Value)
			if err != nil {
				v.statuses[f] = Ambiguous
				v.bad[f] = true
				continue
			}
			v.inv.IssueDate = d
		case FTotalAmount:
			fenVal, err := parseYuan(fv.Value)
			if err != nil {
				v.statuses[f] = Ambiguous
				v.bad[f] = true
				continue
			}
			v.inv.TotalFen, v.amountOK = fenVal, true
		case FBuyerTaxID:
			v.inv.BuyerTaxID = fv.Value
		case FSellerTaxID:
			v.inv.SellerTaxID = fv.Value
		case FBuyerName:
			v.inv.BuyerName = fv.Value
		case FSellerName:
			v.inv.SellerName = fv.Value
		case FItemName:
			v.inv.ItemName = fv.Value
		}
	}
	if v.bad[FInvoiceNo] {
		v.inv.InvoiceNo = ""
	}
	return v
}

func findInvoice(tr ToolOutcome, no string) *InvoiceRecord {
	for i := range tr.Invoices {
		if tr.Invoices[i].InvoiceNo == no {
			return &tr.Invoices[i]
		}
	}
	return nil
}

func findCompany(tr ToolOutcome, taxID string) *CompanyRecord {
	for i := range tr.Companies {
		if tr.Companies[i].TaxID == taxID {
			return &tr.Companies[i]
		}
	}
	return nil
}

func histFilter(tr ToolOutcome, f func(HistoryRecord) bool) []HistoryRecord {
	var out []HistoryRecord
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
	case histApproved:
		return "已通过"
	case histReviewing:
		return "审核中"
	case histRejected:
		return "已驳回"
	}
	return s
}

// FieldCN 返回字段的中文名，供界面与报告使用。
func FieldCN(f FieldName) string {
	switch f {
	case FInvoiceNo:
		return "发票号码"
	case FIssueDate:
		return "开票日期"
	case FTotalAmount:
		return "价税合计"
	case FBuyerTaxID:
		return "购买方税号"
	case FSellerTaxID:
		return "销售方税号"
	case FBuyerName:
		return "购买方名称"
	case FSellerName:
		return "销售方名称"
	case FItemName:
		return "发票项目"
	}
	return string(f)
}

func toolCN(s Status) string {
	switch s {
	case Found, NotFound:
		return "成功或查无"
	case Failed:
		return "失败或未调用"
	case Timeout:
		return "超时"
	}
	return "未知"
}

func categoryCN(c string) string {
	switch c {
	case policy.CatTransport:
		return "交通"
	case policy.CatLodging:
		return "住宿"
	case policy.CatMeal:
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
	if cat == policy.CatLodging {
		return "person"
	}
	return "person"
}

func personCN(cat string) string {
	if cat == policy.CatLodging {
		return "入住人"
	}
	return "乘车人"
}
