package mockgen

import "reimburse/internal/schema"

// violations 构造单项违规用例（每条只触发一个判定项，便于定位规则出错的位置），再加少量多项叠加用例。
func (b *builder) violations() {
	b.violInvoice()
	b.violClaim()
	b.violCombo()
}

// badCheckDigit 把统一社会信用代码的校验位换成另一个字符，使位数合法但校验失败。
func badCheckDigit(id string) string {
	alt := byte('1')
	if id[len(id)-1] == alt {
		alt = '2'
	}
	return id[:len(id)-1] + string(alt)
}

// violInvoice 发票本身的问题：真伪与状态、抬头与税号、内容、重复报销。
func (b *builder) violInvoice() {
	g := b.g

	c := g.newTransport(g.person("P"), "train", "二等座", g.cityOfTier("tier2"), 42000, g.randStart())
	c.World.Invoices[0].Status = schema.InvVoided
	b.add(c, "发票已作废", "R01-a", "violation")

	c = g.newLodging(g.person("P"), g.cityOfTier("tier2"), 2, 36000, g.randStart())
	c.World.Invoices[0].Status = schema.InvRedReverse
	b.add(c, "发票已红字冲销", "R01-a", "violation")

	c = g.newMeal(g.person("M"), g.cityOfTier("tier2"), 4, 0, 36000, g.randStart())
	c.World.Invoices = nil
	b.add(c, "税务平台查无此票", "R01-b", "violation")

	c = g.newTransport(g.person("M"), "flight", "经济舱", g.cityOfTier("tier1"), 98000, g.randStart())
	c.World.Invoices[0].TotalFen = 89000
	b.add(c, "查验库登记金额与发票面额不一致（疑似篡改）", "R01-c", "violation")

	p := g.person("P")
	c = g.newTransport(p, "train", "二等座", g.cityOfTier("tier2"), 31000, g.randStart())
	c.Truth.Invoice.BuyerName, c.Truth.Invoice.BuyerTaxID = p.Name, ""
	c.World.Invoices[0].BuyerTaxID = ""
	b.add(c, "个人抬头，无购买方税号", "R02-a R02-b", "violation")

	c = g.newLodging(g.person("P"), g.cityOfTier("tier2"), 2, 36000, g.randStart())
	c.Truth.Invoice.BuyerName = "星河信息技术有限公司"
	b.add(c, "抬头为公司旧名称，税号一致", "R02-a", "violation")

	c = g.newMeal(g.person("P"), g.cityOfTier("tier2"), 5, 0, 40000, g.randStart())
	c.Truth.Invoice.BuyerTaxID = badCheckDigit(c.Truth.Invoice.BuyerTaxID)
	c.World.Invoices[0].BuyerTaxID = c.Truth.Invoice.BuyerTaxID
	b.add(c, "购买方税号校验位错误", "R02-b", "violation")

	c = g.newLodging(g.person("P"), g.cityOfTier("tier2"), 2, 36000, g.randStart())
	c.Truth.Invoice.ItemName = mealItem
	b.add(c, "住宿报销单，发票项目却是餐饮服务", "R03-a", "violation")

	c = g.newMeal(g.person("M"), g.cityOfTier("tier2"), 2, 2, 120000, g.randStart())
	c.Truth.Support.Lines[0].Name = "53度茅台白酒"
	b.add(c, "招待消费清单含白酒", "R03-b", "violation")

	c = g.newLodging(g.person("P"), g.cityOfTier("tier2"), 2, 36000, g.randStart())
	g.histLike(c, func(h *schema.HistRecord) { h.InvoiceNo = c.Truth.Invoice.InvoiceNo })
	b.add(c, "同一发票号码已有通过的报销记录", "R04-a", "violation")

	c = g.newTransport(g.person("P"), "flight", "经济舱", g.cityOfTier("tier1"), 98000, g.randStart())
	g.histLike(c, func(h *schema.HistRecord) { h.TransportMode = "flight" })
	b.add(c, "机票：同人同销售方同日同额，发票号不同（疑似换票重报）", "R04-b", "violation")

	c = g.newMeal(g.person("M"), g.cityOfTier("tier2"), 4, 3, 120000, g.randStart())
	g.histLike(c, func(h *schema.HistRecord) { h.AmountFen = 110000 })
	b.add(c, "招待拆分：同日同店两笔，各自未到审批线，合计超线", "R05-b", "violation", "split")

	c = g.newMeal(g.person("M"), g.cityOfTier("tier2"), 3, 3, 150000, g.randStart())
	g.histLike(c, func(h *schema.HistRecord) { h.AmountFen = 250000; h.IssueDate = addDays(h.IssueDate, -1) })
	b.add(c, "前一天同店已有2500元招待（已过审批线），本单不算拆分", "", "boundary")
}
