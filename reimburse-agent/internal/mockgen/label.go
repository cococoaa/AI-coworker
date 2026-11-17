package mockgen

import (
	"strings"

	"reimburse/internal/schema"
	"reimburse/plugins/reimburse-audit/policy"
)

// Label 根据结构化真值独立推出命中的判定项、最终结论和核准金额。
// 它只读 Claim、Truth.Invoice、Truth.Support 和 World，不读渲染后的文本，
// 因此与日后"从文本抽取字段再判定"的规则引擎互为独立实现，可以互相印证。
func Label(rf *policy.File, c *schema.Case) (hits []string, decision string, approved int64) {
	p := &rf.Params
	cl, inv, sp, w := &c.Claim, &c.Truth.Invoice, c.Truth.Support, &c.World

	skip := map[string]bool{}
	for _, f := range c.Truth.Unreadable {
		for _, id := range p.FieldDeps[f] {
			skip[id] = true
		}
	}
	failed := map[string]bool{}
	for _, f := range w.Faults {
		key := inv.InvoiceNo
		if f.Tool == "check_company" {
			key = inv.SellerTaxID
		}
		if f.Key == key {
			failed[f.Tool] = true
		}
	}
	for tool := range failed {
		for _, id := range p.ToolDeps[tool] {
			skip[id] = true
		}
	}

	hit := map[string]bool{}
	add := func(id string, cond bool) {
		if cond && !skip[id] {
			hit[id] = true
		}
	}

	// R01 发票真实性：查验库中的登记信息与发票对比。
	var reg *schema.RegInvoice
	for i := range w.Invoices {
		if w.Invoices[i].InvoiceNo == inv.InvoiceNo {
			reg = &w.Invoices[i]
		}
	}
	add("R01-b", reg == nil)
	if reg != nil {
		add("R01-a", reg.Status == schema.InvVoided || reg.Status == schema.InvRedReverse)
		add("R01-c", reg.Status == schema.InvNormal && (reg.IssueDate != inv.IssueDate || reg.TotalFen != inv.TotalFen))
	}

	// R02 购买方
	add("R02-a", inv.BuyerName != p.Company.Name)
	add("R02-b", inv.BuyerTaxID == "" || inv.BuyerTaxID != p.Company.TaxID || !policy.ValidTaxID(inv.BuyerTaxID))

	// R03 内容与类别
	add("R03-a", !strings.HasPrefix(inv.ItemName, p.CategoryItemPrefix[cl.Category]))
	text := inv.ItemName
	if sp != nil {
		for _, l := range sp.Lines {
			text += " " + l.Name
		}
	}
	_, bad := p.ForbiddenHit(text)
	add("R03-b", bad)

	// R04 重复报销
	isTrain := strings.Contains(inv.ItemName, p.TrainTicketKeyword)
	for _, h := range w.History {
		add("R04-a", h.InvoiceNo == inv.InvoiceNo && (h.Status == schema.HistApproved || h.Status == schema.HistReviewing))
		add("R04-b", !isTrain && h.InvoiceNo != inv.InvoiceNo && h.ApplicantID == cl.Applicant.EmployeeID &&
			h.SellerTaxID == inv.SellerTaxID && h.IssueDate == inv.IssueDate && h.AmountFen == inv.TotalFen &&
			h.Status != schema.HistRejected)
	}

	// R05 招待审批线与拆分
	line := p.ApprovalLineFen()
	entertain := cl.Category == policy.CatMeal && cl.ExternalCount > 0
	add("R05-a", entertain && inv.TotalFen >= line && cl.EntertainApprovalNo == "")
	if entertain && inv.TotalFen < line {
		sum, allBelow, related := inv.TotalFen, true, 0
		for _, h := range w.History {
			d := daysBetween(h.IssueDate, inv.IssueDate)
			if d < 0 {
				d = -d
			}
			if h.ApplicantID == cl.Applicant.EmployeeID && h.SellerTaxID == inv.SellerTaxID && h.Category == policy.CatMeal &&
				h.Status != schema.HistRejected && h.InvoiceNo != inv.InvoiceNo && d <= p.SplitWindowDays {
				sum += h.AmountFen
				related++
				if h.AmountFen >= line {
					allBelow = false
				}
			}
		}
		add("R05-b", related > 0 && allBelow && sum >= line)
	}

	// R06 金额三方一致
	add("R06-a", cl.AmountFen > inv.TotalFen)
	if sp != nil {
		add("R06-b", abs64(inv.TotalFen-sp.TotalFen) > p.AmountToleranceFen)
	}

	// R07 时效
	add("R07-a", daysBetween(cl.EndDate, cl.SubmitDate) > p.ReimburseDeadlineDays)

	// R08 日期吻合
	if sp != nil {
		switch cl.Category {
		case policy.CatTransport:
			add("R08-a", sp.Date < addDays(cl.StartDate, -p.TransportDateBufferDays) || sp.Date > addDays(cl.EndDate, p.TransportDateBufferDays))
		case policy.CatLodging:
			add("R08-a", sp.CheckIn < cl.StartDate || sp.CheckOut > cl.EndDate || sp.CheckIn > sp.CheckOut)
		case policy.CatMeal:
			add("R08-a", sp.Date < cl.StartDate || sp.Date > cl.EndDate)
		}
	}

	// R09 席别
	if cl.Category == policy.CatTransport && sp != nil && !p.SeatAllowed(sp.Mode, cl.Applicant.Grade, sp.Seat) {
		add("R09-a", cl.SpecialApprovalNo == "")
		add("R09-b", cl.SpecialApprovalNo != "")
	}

	// R10 住宿限额、R11 餐饮人均
	var caps []int64
	if cl.Category == policy.CatLodging && sp != nil && sp.Nights > 0 {
		capFen := p.LodgingLimitFen(cl.City, cl.Applicant.Grade) * int64(sp.Nights)
		if inv.TotalFen > capFen {
			add("R10-a", true)
			caps = append(caps, capFen)
		}
	}
	if cl.Category == policy.CatMeal {
		if n := int64(cl.InternalCount + cl.ExternalCount); n > 0 {
			capFen := p.MealCapFen(cl.ExternalCount > 0) * n
			if inv.TotalFen > capFen {
				add("R11-a", true)
				caps = append(caps, capFen)
			}
		}
	}

	// R12 本人
	if sp != nil && (cl.Category == policy.CatTransport || cl.Category == policy.CatLodging) {
		add("R12-a", sp.Person != cl.Applicant.Name)
	}

	// R13 销售方状态
	for _, co := range w.Companies {
		if co.TaxID == inv.SellerTaxID {
			add("R13-a", co.Status == schema.CoCanceled || co.Status == schema.CoRevoked)
			add("R13-b", co.Status == schema.CoAbnormal)
		}
	}

	// R14 凭证完整性
	add("R14-a", sp == nil)
	missing := cl.RequestNo == ""
	if cl.Category == policy.CatMeal {
		missing = missing || cl.Purpose == "" || cl.InternalCount+cl.ExternalCount == 0 || (cl.ExternalCount > 0 && cl.ExternalOrg == "")
	}
	add("R14-b", missing)

	// R15 核验失败与低置信度（本规则自身不受 skip 影响）
	if len(failed) > 0 {
		hit["R15-a"] = true
	}
	if len(c.Truth.Unreadable) > 0 {
		hit["R15-b"] = true
	}

	for _, id := range rf.AllCheckIDs() {
		if hit[id] {
			hits = append(hits, id)
		}
	}
	decision = rf.Aggregate(hits)
	switch decision {
	case policy.Approve:
		approved = cl.AmountFen
	case policy.ApproveAdj:
		approved = cl.AmountFen
		for _, cp := range caps {
			approved = min64(approved, cp)
		}
	}
	return hits, decision, approved
}
