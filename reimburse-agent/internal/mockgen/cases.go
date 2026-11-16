package mockgen

import (
	"fmt"
	"sort"
	"strings"

	"reimburse/internal/schema"
)

// builder 顺序构造用例。每条用例先手工写下"预期命中的判定项"，再与 Label 独立推出的结果比对，
// 两者不一致说明用例构造或标注逻辑有错，Cases 会返回错误而不是悄悄放过。
type builder struct {
	g    *Gen
	out  []*schema.Case
	errs []string
}

// Cases 生成全部用例。同一个种子得到同一批用例。
func (g *Gen) Cases() ([]*schema.Case, error) {
	b := &builder{g: g}
	b.clean()
	b.boundary()
	b.violations()
	b.faults()
	if len(b.errs) > 0 {
		return b.out, fmt.Errorf("用例自检失败 %d 处:\n%s", len(b.errs), strings.Join(b.errs, "\n"))
	}
	return b.out, nil
}

// add 补全标准答案、历史库背景数据和文本，自检后收入用例集。want 是空格分隔的预期命中项，空串表示无命中。
func (b *builder) add(c *schema.Case, title, want string, tags ...string) *schema.Case {
	g := b.g
	c.Truth.Title = title
	c.Truth.Tags = append([]string{c.Claim.Category}, tags...)
	g.noiseHistory(c)
	hits, dec, appr := Label(g.RF, c)
	if hits == nil {
		hits = []string{}
	}
	c.Truth.Hits, c.Truth.Decision, c.Truth.ApprovedFen = hits, dec, appr
	g.Render(c, g.n)
	if !equalSets(hits, strings.Fields(want)) {
		b.errs = append(b.errs, fmt.Sprintf("%s %s: 预期命中 [%s]，Label 得到 %v", c.ID, title, want, hits))
	}
	if got := g.RF.Aggregate(hits); got != dec {
		b.errs = append(b.errs, fmt.Sprintf("%s: 结论与命中项聚合不一致", c.ID))
	}
	for _, id := range hits {
		if _, _, ok := g.RF.FindCheck(id); !ok {
			b.errs = append(b.errs, fmt.Sprintf("%s: 判定项 %s 不在规则表中", c.ID, id))
		}
	}
	b.out = append(b.out, c)
	return c
}

// wantApproved 断言核准金额。
func (b *builder) wantApproved(c *schema.Case, fen int64) {
	if c.Truth.ApprovedFen != fen {
		b.errs = append(b.errs, fmt.Sprintf("%s: 核准金额预期 %d 分，得到 %d 分", c.ID, fen, c.Truth.ApprovedFen))
	}
}

// noiseHistory 给历史库加 0 到 2 条与本单无关的记录（其他申请人、其他销售方），让查重工具面对的不是空库。
func (g *Gen) noiseHistory(c *schema.Case) {
	n := g.R.Intn(3)
	cats := []string{"transport", "lodging", "meal"}
	sts := []string{schema.HistApproved, schema.HistApproved, schema.HistRejected}
	for i := 0; i < n; i++ {
		o := pick(g.R, people)
		for o.ID == c.Claim.Applicant.EmployeeID {
			o = pick(g.R, people)
		}
		d := addDays(c.Claim.StartDate, -(15 + g.R.Intn(30)))
		h := schema.HistRecord{
			ClaimNo: fmt.Sprintf("BH%sN%d", c.ID, i+1), InvoiceNo: g.newInvoiceNo(), ApplicantID: o.ID,
			SellerTaxID: g.newTaxID(), IssueDate: d, AmountFen: 20000 + g.R.Int63n(180000),
			Category: pick(g.R, cats), Status: pick(g.R, sts), SubmitDate: addDays(d, 5),
		}
		if h.Category == "transport" {
			h.TransportMode = "train"
		}
		g.addHistory(c, h)
	}
}

// histLike 追加一条与本单发票相似的历史记录（同申请人、同销售方、同日期、同金额、不同发票号、已通过），mut 可进一步修改。
func (g *Gen) histLike(c *schema.Case, mut func(h *schema.HistRecord)) {
	inv, cl := c.Truth.Invoice, c.Claim
	h := schema.HistRecord{
		ClaimNo: fmt.Sprintf("BH%sK%d", c.ID, len(c.World.History)+1), InvoiceNo: g.newInvoiceNo(),
		ApplicantID: cl.Applicant.EmployeeID, SellerTaxID: inv.SellerTaxID, IssueDate: inv.IssueDate,
		AmountFen: inv.TotalFen, Category: cl.Category, Status: schema.HistApproved, SubmitDate: addDays(inv.IssueDate, 5),
	}
	if mut != nil {
		mut(&h)
	}
	g.addHistory(c, h)
}

// swapSeller 把发票销售方换成一家指定状态的新企业。名称须在全部用例中唯一，避免合并后端数据时状态冲突。
func (g *Gen) swapSeller(c *schema.Case, name, city, status string) {
	co := g.seller(name, city)
	co.Status = status
	inv := &c.Truth.Invoice
	inv.SellerName, inv.SellerTaxID = co.Name, co.TaxID
	c.World.Invoices[0].SellerTaxID = co.TaxID
	c.World.Companies = []schema.Company{co}
}

// otherName 返回一位与 p 不同的员工姓名。
func otherName(p Person) string {
	for _, q := range people {
		if q.ID != p.ID {
			return q.Name
		}
	}
	return ""
}

// Coverage 返回用例集命中过的判定项集合，用于检查规则覆盖。
func Coverage(cases []*schema.Case) []string {
	m := map[string]bool{}
	for _, c := range cases {
		for _, h := range c.Truth.Hits {
			m[h] = true
		}
	}
	var ids []string
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
