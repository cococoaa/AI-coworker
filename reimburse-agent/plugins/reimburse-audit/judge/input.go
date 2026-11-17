package judge

// 本文件定义判定引擎的输入/输出，全部是插件自有类型，不引用宿主任何包。
// 宿主把 schema.Claim、工具返回、抽取结果翻译成这里的类型再传进来。

// Claim 是员工提交的报销单（插件视角的字段子集）。
// 只保留判定真正用到的字段，宿主多出来的字段不影响插件。
type Claim struct {
	ClaimNo    string
	RequestNo  string
	Applicant  Applicant
	Category   string // transport / lodging / meal
	FromCity   string
	City       string
	StartDate  string // YYYY-MM-DD
	EndDate    string
	Purpose    string
	AmountFen  int64
	SubmitDate string

	InternalCount int
	ExternalCount int
	ExternalOrg   string

	EntertainApprovalNo string
	SpecialApprovalNo   string

	// SupportFile 为空表示没有上传佐证凭证。
	SupportFile string
}

// Applicant 是申请人的判定相关字段。
type Applicant struct {
	Name       string
	EmployeeID string
	Grade      string // P / M / D
}

// ToolOutcome 是一次核验的结果，含原始记录，供插件判定与界面展示。
// Status 与 Verdict.Status 同义，但额外允许多一个 Skipped：表示这次压根没发起查询。
type ToolOutcome struct {
	Status Verdict

	// Raw 是宿主记录的原始返回文本，插件不解析它，只透传给界面审计。
	Raw string

	Invoices  []InvoiceRecord
	Companies []CompanyRecord
	History   []HistoryRecord
}

// Skipped 构造一个"未发起查询"的结果。
func Skipped(reason string) ToolOutcome {
	return ToolOutcome{Status: Verdict{Status: Failed, Reason: reason}}
}

// Detail 是 Reason 的简写，便于宿主与界面统一读取。
func (t ToolOutcome) Detail() string { return t.Status.Reason }

// OK 报告这次核验能否作为判定依据。
func (t ToolOutcome) OK() bool { return t.Status.Status.Usable() }

// StatusOf 返回四态，便于 switch。
func (t ToolOutcome) StatusOf() Status { return t.Status.Status }

// Input 是判定一次的全部输入。
type Input struct {
	Claim *Claim
	// Fields 是双模型合并后的字段。
	Fields Fields
	// Untrusted 是两模型不一致的字段。
	Untrusted map[FieldName]bool
	// LowConfidence 是置信度不足的字段（不含 Untrusted）。
	LowConfidence []FieldName
	// SupportText 是佐证凭证原文，为空表示没有上传。
	SupportText string
	// Tools 的键为 verify_invoice / check_company / check_history。
	Tools map[string]ToolOutcome
	// Evidence 是宿主额外提供的旁证（例如复核记录），插件只用其数量与状态做判定。
	ExtraHistory []HistoryRecord

	// supportDoc 是佐证凭证的解析缓存，首次访问时解析。
	supportDoc *SupportDoc
}

// Finding 是一条命中的判定项，带可追溯的依据。
type Finding struct {
	CheckID    string `json:"check_id"`
	RuleID     string `json:"rule_id"`
	RuleName   string `json:"rule_name"`
	Clause     string `json:"clause"`
	ClauseText string `json:"clause_text"`
	Outcome    string `json:"outcome"`
	When       string `json:"when"`
	// Reason 是用这张单据的实际数值写成的一句话。
	Reason string `json:"reason"`
	// Evidence 是支撑该判定的数据点（例如某次工具返回的关键字段）。
	Evidence []string `json:"evidence,omitempty"`
}

// Result 是判定结果。
type Result struct {
	Decision    string    `json:"decision"`
	Hits        []string  `json:"hits"`
	Findings    []Finding `json:"findings"`
	ApprovedFen int64     `json:"approved_fen"`
	// ClaimAmountFen 是申请金额，供审核意见核对金额时引用。
	ClaimAmountFen int64 `json:"claim_amount_fen"`
	// Skipped 记录未作判定的判定项及原因。
	Skipped map[string]string `json:"skipped"`
	// Unverified 是因为工具失败/超时/未调用而没有核验的外部事实。
	Unverified []string `json:"unverified,omitempty"`
	// Verified 是已核验通过的外部事实。
	Verified []string `json:"verified,omitempty"`
	// Evidence 是判定过程中用到的原始数值，供意见核对金额时引用。
	Evidence []string `json:"evidence,omitempty"`
}
