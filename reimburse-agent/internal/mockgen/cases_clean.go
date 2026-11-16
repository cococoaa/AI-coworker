package mockgen

// clean 构造材料齐全、应当自动通过的用例。
func (b *builder) clean() {
	g := b.g

	c := g.newTransport(g.person("P"), "train", "二等座", g.cityOfTier("tier2"), 55300, g.randStart())
	b.add(c, "普通员工乘高铁二等座，材料齐全", "", "clean")

	c = g.newTransport(g.person("P"), "flight", "经济舱", g.cityOfTier("tier1"), 128000, g.randStart())
	b.add(c, "普通员工乘飞机经济舱，材料齐全", "", "clean")

	c = g.newLodging(g.person("P"), g.cityOfTier("tier2"), 2, 38000, g.randStart())
	b.add(c, "二线城市住2晚，夜均380元，低于限额400元", "", "clean")
	b.wantApproved(c, 76000)

	c = g.newMeal(g.person("M"), g.cityOfTier("tier2"), 5, 0, 42000, g.randStart())
	b.add(c, "5人工作餐，人均84元", "", "clean")
	b.wantApproved(c, 42000)

	c = g.newMeal(g.person("D"), g.cityOfTier("tier1"), 3, 3, 150000, g.randStart())
	b.add(c, "3内3外业务招待1500元，人均250元，未达审批线", "", "clean")

	c = g.newMeal(g.person("M"), g.cityOfTier("tier2"), 6, 6, 340000, g.randStart())
	c.Claim.EntertainApprovalNo = "ZD2610008"
	b.add(c, "12人招待3400元，达到审批线且已有事前审批号", "", "clean")
}
