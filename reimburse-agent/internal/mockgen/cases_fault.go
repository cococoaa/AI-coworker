package mockgen

import "reimburse/internal/schema"

// faults 构造工具故障与字段不可辨认的用例。共同点：无法核验时依赖它的判定项不作判定，只由 R15 转人工。
func (b *builder) faults() {
	g := b.g

	c := g.newTransport(g.person("P"), "train", "二等座", g.cityOfTier("tier2"), 61000, g.randStart())
	g.addFault(c, "verify_invoice", c.Truth.Invoice.InvoiceNo, schema.FaultTimeout)
	b.add(c, "发票查验超时（状态未知，不能当作通过）", "R15-a", "fault")

	c = g.newLodging(g.person("P"), g.cityOfTier("tier2"), 2, 36000, g.randStart())
	c.World.Invoices[0].Status = schema.InvVoided
	g.addFault(c, "verify_invoice", c.Truth.Invoice.InvoiceNo, schema.FaultGarbled)
	b.add(c, "发票实为作废，但查验返回乱码，只能转人工，不得凭猜测驳回或通过", "R15-a", "fault")

	c = g.newMeal(g.person("P"), g.cityOfTier("tier2"), 4, 0, 36000, g.randStart())
	g.swapSeller(c, "金禾源餐饮服务有限公司", c.Claim.City, schema.CoCanceled)
	g.addFault(c, "check_company", c.Truth.Invoice.SellerTaxID, schema.FaultError)
	b.add(c, "销售方实为注销，但企业核验报错，转人工", "R15-a", "fault")

	c = g.newLodging(g.person("P"), g.cityOfTier("tier2"), 2, 36000, g.randStart())
	g.histLike(c, func(h *schema.HistRecord) { h.InvoiceNo = c.Truth.Invoice.InvoiceNo })
	g.addFault(c, "check_history", c.Truth.Invoice.InvoiceNo, schema.FaultTimeout)
	b.add(c, "发票实为重复报销，但历史查重超时，转人工", "R15-a", "fault")

	c = g.newLodging(g.person("M"), g.cityOfTier("tier1"), 2, 90000, g.randStart())
	c.Truth.Unreadable = []string{"total_amount"}
	b.add(c, "发票金额污损无法辨认，住宿超标项不作判定", "R15-b", "fault")

	c = g.newMeal(g.person("P"), g.cityOfTier("tier2"), 5, 0, 40000, g.randStart())
	c.Truth.Invoice.BuyerTaxID = badCheckDigit(c.Truth.Invoice.BuyerTaxID)
	c.World.Invoices[0].BuyerTaxID = c.Truth.Invoice.BuyerTaxID
	c.Truth.Unreadable = []string{"buyer_tax_id"}
	b.add(c, "购买方税号污损且本身有误，不据污损值驳回，转人工", "R15-b", "fault")

	c = g.newTransport(g.person("P"), "train", "二等座", g.cityOfTier("tier2"), 55300, g.randStart())
	c.World.Invoices[0].Status = schema.InvVoided
	c.Truth.Unreadable = []string{"invoice_no"}
	b.add(c, "发票号码污损，无法查验，转人工", "R15-b", "fault")

	c = g.newLodging(g.person("P"), g.cityOfTier("tier2"), 2, 36000, g.randStart())
	g.swapSeller(c, "盛世通商务酒店管理有限公司", c.Claim.City, schema.CoCanceled)
	c.Truth.Unreadable = []string{"seller_tax_id"}
	b.add(c, "销售方税号污损，无法核验企业状态，转人工", "R15-b", "fault")

	c = g.newTransport(g.person("P"), "train", "二等座", g.cityOfTier("tier2"), 55300, g.randStart())
	c.Truth.Unreadable = []string{"issue_date"}
	b.add(c, "开票日期污损，转人工", "R15-b", "fault")

	c = g.newMeal(g.person("D"), g.cityOfTier("tier1"), 3, 3, 150000, g.randStart())
	g.addFault(c, "verify_invoice", c.Truth.Invoice.InvoiceNo, schema.FaultTimeout)
	g.addFault(c, "check_company", c.Truth.Invoice.SellerTaxID, schema.FaultError)
	c.Truth.Unreadable = []string{"total_amount"}
	b.add(c, "两个工具同时故障且金额污损", "R15-a R15-b", "fault")
}
