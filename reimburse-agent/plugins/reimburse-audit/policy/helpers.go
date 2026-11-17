package policy

import "strings"

// FindCheck 按判定项编号（如 R02-a）查规则与判定项。
func (f *File) FindCheck(id string) (Rule, Check, bool) {
	for _, r := range f.Rules {
		for _, c := range r.Checks {
			if c.ID == id {
				return r, c, true
			}
		}
	}
	return Rule{}, Check{}, false
}

// AllCheckIDs 按规则表顺序返回全部判定项编号。
func (f *File) AllCheckIDs() []string {
	var ids []string
	for _, r := range f.Rules {
		for _, c := range r.Checks {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// Aggregate 按 decision_priority 取命中项中最严格的结果；无命中为 approve。
// 未知编号会被忽略，调用方应先自行校验编号。
func (f *File) Aggregate(hitIDs []string) string {
	best := len(f.Params.DecisionPriority)
	for _, id := range hitIDs {
		_, c, ok := f.FindCheck(id)
		if !ok {
			continue
		}
		for i, o := range f.Params.DecisionPriority {
			if o == c.Outcome && i < best {
				best = i
			}
		}
	}
	if best == len(f.Params.DecisionPriority) {
		return Approve
	}
	return f.Params.DecisionPriority[best]
}

// CityTier 返回城市档次：tier1、tier2，其余为 tier3。
func (p *Params) CityTier(city string) string {
	for _, c := range p.CityTiers.Tier1 {
		if c == city {
			return "tier1"
		}
	}
	for _, c := range p.CityTiers.Tier2 {
		if c == city {
			return "tier2"
		}
	}
	return "tier3"
}

// LodgingLimitFen 返回住宿夜均限额（分）。
func (p *Params) LodgingLimitFen(city, grade string) int64 {
	return p.LodgingLimitYuan[p.CityTier(city)][grade] * 100
}

// MealCapFen 返回餐饮人均上限（分）；有外部人员按招待标准。
func (p *Params) MealCapFen(hasExternal bool) int64 {
	if hasExternal {
		return p.MealPerCapitaYuan.Entertain * 100
	}
	return p.MealPerCapitaYuan.WorkMeal * 100
}

// ApprovalLineFen 返回招待费审批线（分）。
func (p *Params) ApprovalLineFen() int64 { return p.EntertainApprovalLineYuan * 100 }

// SeatAllowed 判断席别是否在该职级允许范围内。mode 为 train 或 flight。
func (p *Params) SeatAllowed(mode, grade, seat string) bool {
	m := p.TrainSeats
	if mode == "flight" {
		m = p.FlightSeats
	}
	for _, s := range m[grade] {
		if s == seat {
			return true
		}
	}
	return false
}

// ForbiddenHit 返回文本中命中的第一个禁止关键词。
func (p *Params) ForbiddenHit(text string) (string, bool) {
	for _, k := range p.ForbiddenKeywords {
		if strings.Contains(text, k) {
			return k, true
		}
	}
	return "", false
}
