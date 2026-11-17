package mockgen

import (
	"fmt"
	"math/rand"

	"reimburse/internal/schema"
	"reimburse/plugins/reimburse-audit/policy"
)

// Person 是 mock 员工。职级 P 普通员工，M 经理，D 总监。
type Person struct{ Name, ID, Grade string }

var people = []Person{
	{"陈思远", "E1001", "P"}, {"林晓雯", "E1002", "P"}, {"王子轩", "E1003", "P"},
	{"赵雅琪", "E1004", "P"}, {"刘子豪", "E1005", "P"},
	{"周明辉", "E2001", "M"}, {"吴静怡", "E2002", "M"}, {"郑浩然", "E2003", "M"},
	{"孙立群", "E3001", "D"}, {"何婉清", "E3002", "D"},
}

var tier3Cities = []string{"无锡", "济南", "福州", "南昌", "昆明", "南宁", "烟台", "常州", "珠海", "大连"}
var hotelBrands = []string{"锦程", "栖云", "望岳", "云舍", "悦途", "安澜"}
var restBrands = []string{"江南小馆", "川味坊", "老街味道", "海味轩", "食尚汇", "聚贤楼"}
var purposes = []string{"客户项目现场实施", "参加行业技术交流会", "供应商现场评审", "项目验收与回访", "培训与技术支持"}
var mealPurposes = []string{"项目组加班工作餐", "部门周例会工作餐", "版本发布后团队聚餐"}
var entertainPurposes = []string{"接待客户商务洽谈", "接待供应商技术交流", "接待合作方项目评审"}
var externalOrgs = []string{"远景新能源有限公司", "汇通物流集团有限公司", "启明医疗科技有限公司"}
var dishes = []string{"红烧鲫鱼", "清蒸鲈鱼", "宫保鸡丁", "农家小炒肉", "时令蔬菜", "蒜蓉粉丝虾", "酸辣汤", "米饭", "饮料", "果盘", "凉拌黄瓜", "干锅花菜"}

const (
	trainSeller  = "东海铁路客运服务有限公司"
	flightSeller = "云帆航空有限责任公司"
	trainItem    = "*运输服务*铁路旅客运输"
	flightItem   = "*运输服务*航空旅客运输"
	lodgingItem  = "*住宿服务*住宿费"
	mealItem     = "*餐饮服务*餐费"
)

// Gen 保存生成过程的全部状态。同一个种子必然得到同一批用例。
type Gen struct {
	R       *rand.Rand
	RF      *policy.File
	seed    int64
	sellers map[string]schema.Company
	usedNo  map[string]bool
	usedTax map[string]bool
	n       int
}

func NewGen(seed int64, rf *policy.File) *Gen {
	return &Gen{R: rand.New(rand.NewSource(seed)), RF: rf, seed: seed,
		sellers: map[string]schema.Company{}, usedNo: map[string]bool{}, usedTax: map[string]bool{}}
}

// person 随机取一位指定职级的员工。
func (g *Gen) person(grade string) Person {
	var ps []Person
	for _, p := range people {
		if p.Grade == grade {
			ps = append(ps, p)
		}
	}
	return pick(g.R, ps)
}

// cityOfTier 随机取一个指定档次的出差城市（不含常驻地上海）。
func (g *Gen) cityOfTier(tier string) string {
	switch tier {
	case "tier1":
		var cs []string
		for _, c := range g.RF.Params.CityTiers.Tier1 {
			if c != "上海" {
				cs = append(cs, c)
			}
		}
		return pick(g.R, cs)
	case "tier2":
		return pick(g.R, g.RF.Params.CityTiers.Tier2)
	}
	return pick(g.R, tier3Cities)
}

// randStart 返回 2026-09-21 到 2026-10-28 之间的随机日期。
func (g *Gen) randStart() string { return addDays("2026-09-21", g.R.Intn(38)) }

func (g *Gen) newTaxID() string {
	cs := policy.TaxIDCharset()
	for {
		b := []byte(fmt.Sprintf("913101%02d", g.R.Intn(20)))
		for i := 0; i < 9; i++ {
			b = append(b, cs[g.R.Intn(len(cs))])
		}
		c, _ := policy.TaxIDCheckChar(string(b))
		id := string(b) + string(c)
		if id != g.RF.Params.Company.TaxID && !g.usedTax[id] {
			g.usedTax[id] = true
			return id
		}
	}
}

func (g *Gen) newInvoiceNo() string {
	for {
		s := fmt.Sprintf("26%02d%016d", 11+g.R.Intn(40), g.R.Int63n(10000000000000000))
		if !g.usedNo[s] {
			g.usedNo[s] = true
			return s
		}
	}
}

// seller 按名称取销售方，首次出现时登记税号。同一名称永远对应同一税号。
func (g *Gen) seller(name, city string) schema.Company {
	if c, ok := g.sellers[name]; ok {
		return c
	}
	c := schema.Company{TaxID: g.newTaxID(), Name: name, City: city, Status: schema.CoActive}
	g.sellers[name] = c
	return c
}

func (g *Gen) hotelSeller(city string) (schema.Company, string) {
	b := pick(g.R, hotelBrands)
	return g.seller(city+b+"酒店管理有限公司", city), fmt.Sprintf("%s酒店(%s中心店)", b, city)
}

func (g *Gen) restSeller(city string) (schema.Company, string) {
	b := pick(g.R, restBrands)
	return g.seller(city+b+"餐饮管理有限公司", city), city + b
}
