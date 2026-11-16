package mockgen

import "reimburse/internal/schema"

// boundary 构造恰好卡在阈值两侧的用例：规则代码里最容易写错 > 与 >= 的地方。
func (b *builder) boundary() {
	g := b.g

	c := g.newLodging(g.person("P"), g.cityOfTier("tier3"), 2, 30000, g.randStart())
	b.add(c, "三线城市普通员工夜均恰为限额300元，不算超标", "", "boundary")
	b.wantApproved(c, 60000)

	c = g.newLodging(g.person("P"), g.cityOfTier("tier1"), 1, 50001, g.randStart())
	b.add(c, "一线城市普通员工夜均超限额1分，按限额核准", "R10-a", "boundary")
	b.wantApproved(c, 50000)

	c = g.newMeal(g.person("P"), g.cityOfTier("tier2"), 2, 2, 120000, g.randStart())
	b.add(c, "4人招待人均恰为上限300元，不算超标", "", "boundary")
	b.wantApproved(c, 120000)

	c = g.newTransport(g.person("P"), "train", "二等座", g.cityOfTier("tier2"), 48600, g.randStart())
	late(c, 30)
	b.add(c, "提交日距出差结束日恰好30天，不算超期", "", "boundary")

	c = g.newTransport(g.person("P"), "train", "二等座", g.cityOfTier("tier2"), 48600, g.randStart())
	late(c, 31)
	b.add(c, "提交日距出差结束日31天，超期", "R07-a", "boundary")

	c = g.newMeal(g.person("M"), g.cityOfTier("tier2"), 4, 4, 199999, g.randStart())
	b.add(c, "招待1999.99元，差1分到审批线，无审批号也不需要", "", "boundary")

	c = g.newMeal(g.person("M"), g.cityOfTier("tier2"), 4, 4, 200000, g.randStart())
	b.add(c, "招待恰为2000元，达到审批线，缺事前审批号", "R05-a", "boundary")

	c = g.newTransport(g.person("P"), "train", "二等座", g.cityOfTier("tier2"), 47000, g.randStart())
	g.moveDate(c, addDays(c.Claim.StartDate, -1))
	b.add(c, "乘车日早于出差开始日1天，在交通缓冲期内", "", "boundary")

	c = g.newTransport(g.person("P"), "train", "二等座", g.cityOfTier("tier2"), 55300, g.randStart())
	g.histLike(c, func(h *schema.HistRecord) { h.TransportMode = "train" })
	b.add(c, "火车票往返同日同额同销售方，号码不同，属正常往返不判重复", "", "boundary")

	c = g.newMeal(g.person("M"), g.cityOfTier("tier2"), 3, 3, 150000, g.randStart())
	g.histLike(c, func(h *schema.HistRecord) { h.AmountFen = 120000; h.IssueDate = addDays(h.IssueDate, -3) })
	b.add(c, "招待1500元，3天前同店已报1200元，窗口内合计超审批线", "R05-b", "boundary")

	c = g.newMeal(g.person("M"), g.cityOfTier("tier2"), 3, 3, 150000, g.randStart())
	g.histLike(c, func(h *schema.HistRecord) { h.AmountFen = 120000; h.IssueDate = addDays(h.IssueDate, -4) })
	b.add(c, "招待1500元，4天前同店已报1200元，已出窗口不合并", "", "boundary")

	c = g.newTransport(g.person("D"), "flight", "公务舱", g.cityOfTier("tier1"), 265000, g.randStart())
	b.add(c, "总监乘公务舱，职级允许", "", "boundary")

	c = g.newLodging(g.person("P"), g.cityOfTier("tier2"), 2, 35000, g.randStart())
	g.histLike(c, func(h *schema.HistRecord) { h.InvoiceNo = c.Truth.Invoice.InvoiceNo; h.Status = schema.HistRejected })
	b.add(c, "同一发票此前被驳回，修正后重新提交，不算重复", "", "boundary")
}
