// Package policy 加载并校验报销制度数据（policy.json）。
// 规则表是唯一来源：mock 生成器、规则引擎、文档生成器都读同一份数据。
package policy

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

//go:embed policy.json
var embedded []byte

//go:embed presets/municipal-soe/policy.json
var municipalDemo []byte

// 判定结果类型，按 decision_priority 从严到宽排序。
const (
	Approve      = "approve"
	ApproveAdj   = "approve_with_adjustment"
	Reject       = "reject"
	ManualReview = "manual_review"
	NeedMoreInfo = "need_more_info"
)

// 报销类别。
const (
	CatTransport = "transport"
	CatLodging   = "lodging"
	CatMeal      = "meal"
)

type Policy struct {
	Name       string   `json:"name"`
	Version    string   `json:"version"`
	Disclaimer string   `json:"disclaimer"`
	Scope      string   `json:"scope"`
	Notes      []string `json:"notes"`
}

type Params struct {
	Company struct {
		Name  string `json:"name"`
		TaxID string `json:"tax_id"`
	} `json:"company"`
	DecisionPriority        []string            `json:"decision_priority"`
	ReimburseDeadlineDays   int                 `json:"reimburse_deadline_days"`
	TransportDateBufferDays int                 `json:"transport_date_buffer_days"`
	LowConfidenceThreshold  float64             `json:"low_confidence_threshold"`
	AmountToleranceFen      int64               `json:"amount_tolerance_fen"`
	KeyFields               []string            `json:"key_fields"`
	CategoryItemPrefix      map[string]string   `json:"category_item_prefix"`
	ForbiddenKeywords       []string            `json:"forbidden_keywords"`
	TrainSeats              map[string][]string `json:"train_seats"`
	FlightSeats             map[string][]string `json:"flight_seats"`
	CityTiers               struct {
		Tier1 []string `json:"tier1"`
		Tier2 []string `json:"tier2"`
	} `json:"city_tiers"`
	LodgingLimitYuan  map[string]map[string]int64 `json:"lodging_limit_yuan"`
	MealPerCapitaYuan struct {
		WorkMeal  int64 `json:"work_meal"`
		Entertain int64 `json:"entertain"`
	} `json:"meal_per_capita_yuan"`
	EntertainApprovalLineYuan int64 `json:"entertain_approval_line_yuan"`
	SplitWindowDays           int   `json:"split_window_days"`
	// TrainTicketKeyword 出现在发票项目名称中即视为火车票（R04-b 对火车票不适用）。
	TrainTicketKeyword string `json:"train_ticket_keyword"`
	// FieldDeps：某个关键字段不可信时，依赖它的判定项不作判定。
	FieldDeps map[string][]string `json:"field_deps"`
	// ToolDeps：某个工具失败、超时时，依赖它的判定项不作判定。
	ToolDeps map[string][]string `json:"tool_deps"`
}

type Check struct {
	ID      string `json:"id"`
	When    string `json:"when"`
	Outcome string `json:"outcome"`
	Adjust  string `json:"adjust,omitempty"`
	// Requires 声明该判定项依赖的字段、工具与凭证字段。
	// 依赖跟着规则写在一起，新增规则时不会忘；缺省时从 params.field_deps / tool_deps 反推。
	Requires Requires `json:"requires,omitempty"`
}

// Requires 是一条判定项的依赖声明。
type Requires struct {
	// Fields 是必须可信的抽取字段名；任一不可信则本条不作判定。
	Fields []string `json:"fields,omitempty"`
	// Tools 是必须可用的核验工具；任一失败或超时则本条不作判定。
	Tools []string `json:"tools,omitempty"`
	// Support 是必须存在的佐证凭证字段（total/date/check_in/check_out/seat/person/nights）。
	Support []string `json:"support,omitempty"`
}

type Rule struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Clause     string   `json:"clause"`
	ClauseText string   `json:"clause_text"`
	Basis      []string `json:"basis"`
	Note       string   `json:"note,omitempty"`
	AppliesTo  []string `json:"applies_to"`
	Tools      []string `json:"tools"`
	Checks     []Check  `json:"checks"`
}

type File struct {
	// Schema 与 PluginVersion 是元信息，不参与判定。
	Schema        string `json:"$schema,omitempty"`
	PluginVersion string `json:"plugin_version,omitempty"`
	Policy        Policy `json:"policy"`
	Params        Params `json:"params"`
	Rules         []Rule `json:"rules"`
}

// Load 读取内嵌的规则表。
func Load() (*File, error) { return parse(embedded) }

// LoadMunicipalDemo loads the packaged, fictional municipal enterprise policy.
func LoadMunicipalDemo() (*File, error) { return parse(municipalDemo) }

// LoadFile 读取外部规则表文件（便于现场调整阈值）。
func LoadFile(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parse(b)
}

func parse(b []byte) (*File, error) {
	var f File
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("解析规则表: %w", err)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// Validate 检查规则表自身的一致性。
func (f *File) Validate() error {
	valid := map[string]bool{}
	for _, o := range f.Params.DecisionPriority {
		valid[o] = true
	}
	for _, o := range []string{Approve, ApproveAdj, Reject, ManualReview, NeedMoreInfo} {
		if !valid[o] {
			return fmt.Errorf("decision_priority 缺少 %s", o)
		}
	}
	seenRule, seenCheck := map[string]bool{}, map[string]bool{}
	for _, r := range f.Rules {
		if seenRule[r.ID] {
			return fmt.Errorf("规则编号重复: %s", r.ID)
		}
		seenRule[r.ID] = true
		if r.Clause == "" || len(r.Checks) == 0 {
			return fmt.Errorf("规则 %s 缺少条款编号或判定项", r.ID)
		}
		for _, c := range r.Checks {
			if !strings.HasPrefix(c.ID, r.ID+"-") || seenCheck[c.ID] {
				return fmt.Errorf("判定项编号不合法或重复: %s", c.ID)
			}
			seenCheck[c.ID] = true
			if !valid[c.Outcome] || c.Outcome == Approve {
				return fmt.Errorf("判定项 %s 的结果类型不合法: %s", c.ID, c.Outcome)
			}
		}
	}
	keys := map[string]bool{}
	for _, k := range f.Params.KeyFields {
		keys[k] = true
	}
	for field, ids := range f.Params.FieldDeps {
		if !keys[field] {
			return fmt.Errorf("field_deps 的字段不在 key_fields 中: %s", field)
		}
		for _, id := range ids {
			if !seenCheck[id] {
				return fmt.Errorf("field_deps[%s] 引用了不存在的判定项: %s", field, id)
			}
		}
	}
	tools := map[string]bool{"verify_invoice": true, "check_company": true, "check_history": true}
	for tool, ids := range f.Params.ToolDeps {
		if !tools[tool] {
			return fmt.Errorf("tool_deps 含未知工具: %s", tool)
		}
		for _, id := range ids {
			if !seenCheck[id] {
				return fmt.Errorf("tool_deps[%s] 引用了不存在的判定项: %s", tool, id)
			}
		}
	}
	return nil
}
