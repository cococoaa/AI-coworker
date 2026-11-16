package mockgen

import "reimburse/internal/schema"

// otherPerson 返回一位与 p 不同的员工。
func otherPerson(p Person) Person {
	for _, q := range people {
		if q.ID != p.ID {
			return q
		}
	}
	return p
}

// moveDate 把费用发生日（乘车日或用餐日）连同发票开票日、查验库登记日一起改到 d，保持三者一致。
func (g *Gen) moveDate(c *schema.Case, d string) {
	c.Truth.Invoice.IssueDate = d
	c.World.Invoices[0].IssueDate = d
	if s := c.Truth.Support; s != nil && s.Kind != "folio" {
		s.Date = d
	}
}

// late 把提交日设为申请单结束日之后的第 days 天。
func late(c *schema.Case, days int) {
	c.Claim.SubmitDate = addDays(c.Claim.EndDate, days)
}
