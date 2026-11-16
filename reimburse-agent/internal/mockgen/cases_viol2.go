package mockgen

import "reimburse/internal/schema"

// violClaim 报销单与佐证凭证层面的问题：金额、日期、席别、超标、本人、销售方状态、凭证齐全。
func (b *builder) violClaim() {
	g := b.g

	c := g.newLodging(g.person("P"), g.cityOfTier("tier2"), 2, 36000, g.randStart())
	c.Claim.AmountFen = 77000
	b.add(c, "申请金额比发票多50元", "R06-a", "violation")

	c = g.newTransport(g.person("P"), "train", "二等座", g.cityOfTier("tier2"), 55300, g.randStart())
	c.Truth.Support.TotalFen = 50300
	g.rebuildSupport(c)
	b.add(c, "行程单合计与发票价税合计不一致", "R06-b", "violation")

	c = g.newTransport(g.person("P"), "train", "二等座", g.cityOfTier("tier2"), 47000, g.randStart())
	g.moveDate(c, addDays(c.Claim.StartDate, -3))
	b.add(c, "乘车日早于出差开始日3天，超出缓冲期", "R08-a", "violation")

	c = g.newLodging(g.person("P"), g.cityOfTier("tier2"), 3, 35000, g.randStart())
	c.Claim.EndDate = addDays(c.Claim.EndDate, -1)
	b.add(c, "离店日晚于申请单结束日", "R08-a", "violation")

	c = g.newTransport(g.person("P"), "train", "一等座", g.cityOfTier("tier2"), 92000, g.randStart())
	b.add(c, "普通员工乘一等座，无特批", "R09-a", "violation")

	c = g.newTransport(g.person("P"), "train", "一等座", g.cityOfTier("tier2"), 92000, g.randStart())
	c.Claim.SpecialApprovalNo = "TB2610021"
	b.add(c, "普通员工乘一等座，填有特批编号（真伪待人工确认）", "R09-b", "violation")

	c = g.newMeal(g.person("M"), g.cityOfTier("tier2"), 4, 0, 56000, g.randStart())
	b.add(c, "工作餐人均140元，超过100元", "R11-a", "violation")
	b.wantApproved(c, 40000)

	c = g.newMeal(g.person("M"), g.cityOfTier("tier2"), 2, 2, 140000, g.randStart())
	b.add(c, "招待人均350元，超过300元", "R11-a", "violation")
	b.wantApproved(c, 120000)

	p := g.person("P")
	c = g.newTransport(p, "train", "二等座", g.cityOfTier("tier2"), 55300, g.randStart())
	c.Truth.Support.Person = otherName(p)
	b.add(c, "行程单乘车人不是申请人", "R12-a", "violation")

	c = g.newMeal(g.person("P"), g.cityOfTier("tier2"), 3, 0, 24000, g.randStart())
	g.swapSeller(c, "鸿运来餐饮服务有限公司", c.Claim.City, schema.CoCanceled)
	b.add(c, "销售方已注销", "R13-a", "violation")

	c = g.newLodging(g.person("P"), g.cityOfTier("tier2"), 2, 36000, g.randStart())
	g.swapSeller(c, "海岚商务酒店管理有限公司", c.Claim.City, schema.CoAbnormal)
	b.add(c, "销售方列入经营异常", "R13-b", "violation")

	c = g.newTransport(g.person("P"), "train", "二等座", g.cityOfTier("tier2"), 55300, g.randStart())
	g.dropSupport(c)
	b.add(c, "缺行程单", "R14-a", "violation")

	c = g.newTransport(g.person("P"), "train", "二等座", g.cityOfTier("tier2"), 55300, g.randStart())
	c.Claim.RequestNo = ""
	b.add(c, "报销单缺申请单号", "R14-b", "violation")

	c = g.newMeal(g.person("M"), g.cityOfTier("tier2"), 2, 2, 100000, g.randStart())
	c.Claim.ExternalOrg = ""
	b.add(c, "招待餐饮缺招待对象单位", "R14-b", "violation")
}

// violCombo 多项叠加，检查结论按优先级取最严格者。
func (b *builder) violCombo() {
	g := b.g

	c := g.newLodging(g.person("P"), g.cityOfTier("tier2"), 2, 36000, g.randStart())
	g.swapSeller(c, "远洲商务酒店管理有限公司", c.Claim.City, schema.CoCanceled)
	c.Truth.Invoice.BuyerName = "星河信息技术有限公司"
	c.World.Invoices[0].Status = schema.InvVoided
	b.add(c, "叠加：发票作废、抬头为旧名称、销售方注销", "R01-a R02-a R13-a", "combo")

	c = g.newTransport(g.person("P"), "train", "一等座", g.cityOfTier("tier2"), 92000, g.randStart())
	c.Claim.RequestNo = ""
	late(c, 45)
	b.add(c, "叠加：超期、席别超标、缺申请单号，三项都是补材料", "R07-a R09-a R14-b", "combo")

	p := g.person("P")
	c = g.newLodging(p, g.cityOfTier("tier1"), 1, 62000, g.randStart())
	c.Truth.Support.Person = otherName(p)
	b.add(c, "叠加：住宿超标（本可调整）又疑似代订，转人工压过调整", "R10-a R12-a", "combo")

	c = g.newMeal(g.person("D"), g.cityOfTier("tier1"), 3, 3, 250000, g.randStart())
	b.add(c, "叠加：招待人均超标且达审批线无审批号，补材料压过调整", "R05-a R11-a", "combo")
}
