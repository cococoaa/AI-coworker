package mockgen

import (
	"fmt"
	"strings"

	"reimburse/internal/schema"
)

// splitTax 由价税合计反推不含税金额和税额（分，四舍五入）。
func splitTax(total int64, rate int) (ex, tax int64) {
	d := int64(100 + rate)
	ex = (total*100 + d/2) / d
	return ex, total - ex
}

func (g *Gen) newCase() *schema.Case {
	g.n++
	id := fmt.Sprintf("C%02d", g.n)
	c := &schema.Case{ID: id}
	c.Truth.ID = id
	c.Claim.ClaimNo = fmt.Sprintf("BX2611%04d", g.n)
	c.Claim.RequestNo = fmt.Sprintf("SQ2610%04d", 100+g.n)
	c.Claim.Attachments.InvoiceFile = "invoice.txt"
	c.Claim.Attachments.SupportFile = "support.txt"
	return c
}

func (g *Gen) invoice(co schema.Company, item string, rate int, total int64, date string) schema.Invoice {
	ex, tax := splitTax(total, rate)
	cp := g.RF.Params.Company
	return schema.Invoice{InvoiceNo: g.newInvoiceNo(), IssueDate: date, BuyerName: cp.Name, BuyerTaxID: cp.TaxID,
		SellerName: co.Name, SellerTaxID: co.TaxID, ItemName: item, TaxRatePct: rate, ExTaxFen: ex, TaxFen: tax, TotalFen: total}
}

// register 把发票和销售方登记到 mock 工具的后端数据，状态均为正常。
func (g *Gen) register(c *schema.Case, co schema.Company) {
	inv := c.Truth.Invoice
	c.World.Invoices = []schema.RegInvoice{{InvoiceNo: inv.InvoiceNo, IssueDate: inv.IssueDate, TotalFen: inv.TotalFen,
		BuyerTaxID: inv.BuyerTaxID, SellerTaxID: inv.SellerTaxID, Status: schema.InvNormal}}
	c.World.Companies = []schema.Company{co}
}

func (g *Gen) claimBase(c *schema.Case, p Person, cat, city, start, end string, amount int64) {
	c.Claim.Applicant = schema.Applicant{Name: p.Name, EmployeeID: p.ID, Grade: p.Grade}
	c.Claim.Category, c.Claim.City, c.Claim.StartDate, c.Claim.EndDate, c.Claim.AmountFen = cat, city, start, end, amount
	c.Claim.SubmitDate = addDays(end, 4+g.R.Intn(7))
}

// newTransport 构造一张干净的交通报销单：乘车日为出差第一天。
func (g *Gen) newTransport(p Person, mode, seat, dest string, total int64, start string) *schema.Case {
	c := g.newCase()
	end := addDays(start, 2)
	seller, item, veh := trainSeller, trainItem, fmt.Sprintf("G%d", 1000+g.R.Intn(8000))
	if mode == "flight" {
		seller, item, veh = flightSeller, flightItem, fmt.Sprintf("YF%d", 1000+g.R.Intn(8000))
	}
	co := g.seller(seller, "上海")
	g.claimBase(c, p, "transport", dest, start, end, total)
	c.Claim.FromCity = "上海"
	c.Claim.Purpose = pick(g.R, purposes)
	c.Truth.Invoice = g.invoice(co, item, 9, total, start)
	c.Truth.Support = &schema.Support{Kind: "itinerary", Mode: mode, Person: p.Name, Date: start,
		Vehicle: veh, From: "上海", To: dest, Seat: seat, TotalFen: total}
	g.rebuildSupport(c)
	g.register(c, co)
	return c
}

// newLodging 构造一张干净的住宿报销单：入住日为出差第一天，离店日为最后一天。
func (g *Gen) newLodging(p Person, city string, nights int, nightlyFen int64, start string) *schema.Case {
	c := g.newCase()
	end := addDays(start, nights)
	total := nightlyFen * int64(nights)
	co, hotel := g.hotelSeller(city)
	g.claimBase(c, p, "lodging", city, start, end, total)
	c.Claim.FromCity = "上海"
	c.Claim.Purpose = pick(g.R, purposes)
	c.Truth.Invoice = g.invoice(co, lodgingItem, 6, total, end)
	c.Truth.Support = &schema.Support{Kind: "folio", Person: p.Name, Hotel: hotel, City: city,
		CheckIn: start, CheckOut: end, Nights: nights, TotalFen: total}
	g.rebuildSupport(c)
	g.register(c, co)
	return c
}

// newMeal 构造一张干净的餐饮报销单。external 大于 0 时按业务招待处理。
func (g *Gen) newMeal(p Person, city string, internal, external int, total int64, date string) *schema.Case {
	c := g.newCase()
	co, venue := g.restSeller(city)
	g.claimBase(c, p, "meal", city, date, date, total)
	c.Claim.InternalCount, c.Claim.ExternalCount = internal, external
	if external > 0 {
		c.Claim.Purpose = pick(g.R, entertainPurposes)
		c.Claim.ExternalOrg = pick(g.R, externalOrgs)
	} else {
		c.Claim.Purpose = pick(g.R, mealPurposes)
	}
	c.Truth.Invoice = g.invoice(co, mealItem, 6, total, date)
	c.Truth.Support = &schema.Support{Kind: "consumption", Venue: venue, City: city, Date: date, TotalFen: total}
	g.rebuildSupport(c)
	g.register(c, co)
	return c
}

// rebuildSupport 按佐证凭证的合计重新生成明细行，使明细之和等于合计。
func (g *Gen) rebuildSupport(c *schema.Case) {
	s := c.Truth.Support
	if s == nil {
		return
	}
	t := s.TotalFen
	switch s.Kind {
	case "itinerary":
		if s.Mode == "flight" {
			const fund, fuel = 5000, 4000
			s.Lines = []schema.Line{{Name: "票价", AmountFen: t - fund - fuel}, {Name: "燃油附加费", AmountFen: fuel}, {Name: "民航发展基金", AmountFen: fund}}
		} else {
			s.Lines = []schema.Line{{Name: "票价", AmountFen: t}}
		}
	case "folio":
		n := int64(s.Nights)
		per := t / n
		s.Lines = nil
		for i := int64(0); i < n; i++ {
			amt := per
			if i == 0 {
				amt = t - per*(n-1)
			}
			s.Lines = append(s.Lines, schema.Line{Date: addDays(s.CheckIn, int(i)), Name: "房费", AmountFen: amt})
		}
	case "consumption":
		k := 4 + g.R.Intn(3)
		names := shuffled(g.R, dishes)[:k]
		w := make([]int64, k)
		var sw int64
		for i := range w {
			w[i] = int64(1 + g.R.Intn(5))
			sw += w[i]
		}
		s.Lines = nil
		var used int64
		for i, nm := range names {
			amt := t * w[i] / sw
			if i == k-1 {
				amt = t - used
			}
			used += amt
			s.Lines = append(s.Lines, schema.Line{Name: nm, Qty: 1 + g.R.Intn(2), AmountFen: amt})
		}
	}
}

// setTotal 把发票、查验库、报销单、佐证凭证的金额同步改成同一个值。
func (g *Gen) setTotal(c *schema.Case, fen int64) {
	inv := &c.Truth.Invoice
	inv.TotalFen = fen
	inv.ExTaxFen, inv.TaxFen = splitTax(fen, inv.TaxRatePct)
	c.World.Invoices[0].TotalFen = fen
	c.Claim.AmountFen = fen
	if s := c.Truth.Support; s != nil {
		s.TotalFen = fen
		g.rebuildSupport(c)
	}
}

// dropSupport 模拟员工没有上传佐证凭证。
func (g *Gen) dropSupport(c *schema.Case) {
	c.Truth.Support = nil
	c.Claim.Attachments.SupportFile = ""
}

// addHistory 向用例的历史报销库追加一条记录。
func (g *Gen) addHistory(c *schema.Case, h schema.HistRecord) {
	c.World.History = append(c.World.History, h)
}

// addFault 在用例中预置一次工具故障。
func (g *Gen) addFault(c *schema.Case, tool, key, mode string) {
	c.World.Faults = append(c.World.Faults, schema.Fault{Tool: tool, Key: key, Mode: mode})
}

func hasTag(tags []string, t string) bool {
	for _, x := range tags {
		if strings.EqualFold(x, t) {
			return true
		}
	}
	return false
}
