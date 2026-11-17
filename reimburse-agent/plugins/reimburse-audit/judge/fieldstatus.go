package judge

import "fmt"

// 字段可信度：把"没读到"和"读到但不确定"分开。
//
// 为什么必须分开——C24 那个案例暴露的问题：
//   发票上确实没有购买方税号（业务事实），模型正确读出了"空"，
//   但因为对"空"这个结果没把握，给了 0.5 的置信度。
//   系统把"置信度低"一律当成"识别失败"，于是跳过 R02-b（税号缺失应驳回），
//   改由 R15-b 兜底转人工。结论仍对，但**依据错了**：该说是"没税号"，
//   却说成"读不清税号"，把业务问题和工程问题混为一谈。
//
// 四态：
//
//	Present      读到了可信的值            可据此判定
//	Absent       确认没有（原件确实留空）  可据此判定 —— 缺字段本身是业务事实
//	Unreadable   看不清/读不出            不可据此判定，需补件或人工
//	Ambiguous    两模型看到的/理解的不一致  不可据此判定，需人工
//
// 关键区别：Absent 与 Unreadable 都会让 Value 为空，但前者能判定、后者不能。

// FieldStatus 是单个字段的可信度状态。
type FieldStatus uint8

const (
	// Unknown 未评估（零值），按不可用处理，避免漏判。
	Unknown FieldStatus = iota
	// Present 读到了可信的值。
	Present
	// Absent 确认原件没有这一项（业务事实，不是故障）。
	Absent
	// Unreadable 看不清、模糊、被遮挡，读不出。
	Unreadable
	// Ambiguous 两侧模型结论不一致，或格式无法解析。
	Ambiguous
)

func (s FieldStatus) String() string {
	switch s {
	case Present:
		return "present"
	case Absent:
		return "absent"
	case Unreadable:
		return "unreadable"
	case Ambiguous:
		return "ambiguous"
	}
	return "unknown"
}

// Usable 表示能否作为判定依据。
// Absent 可用——"这张票没有税号"是可判定的业务事实。
func (s FieldStatus) Usable() bool { return s == Present || s == Absent }

// NeedsEvidence 表示是否需要补件/人工确认才能判定。
func (s FieldStatus) NeedsEvidence() bool { return s == Unreadable || s == Ambiguous }

// MarshalJSON 序列化成字符串，便于界面与日志阅读。
func (s FieldStatus) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

// UnmarshalJSON 同时接受字符串与数字（向前兼容）。
func (s *FieldStatus) UnmarshalJSON(b []byte) error {
	t := string(b)
	trim := func(x string) string {
		if len(x) >= 2 && x[0] == '"' && x[len(x)-1] == '"' {
			return x[1 : len(x)-1]
		}
		return x
	}
	switch trim(t) {
	case "present", "1":
		*s = Present
	case "absent", "2":
		*s = Absent
	case "unreadable", "3":
		*s = Unreadable
	case "ambiguous", "4":
		*s = Ambiguous
	default:
		*s = Unknown
	}
	return nil
}

// StatusLabel 是给财务看的中文说明。区分"原件没有"与"我们看不清"是重点。
func StatusLabel(name FieldName, st FieldStatus) string {
	field := FieldCN(name)
	switch st {
	case Present:
		return field + "已读到"
	case Absent:
		return field + "原件未填写"
	case Unreadable:
		return field + "无法识别（看不清或缺失关键字符）"
	case Ambiguous:
		return field + "两个模型结果不一致"
	}
	return field + "未评估"
}

// AbsenceReason 说明一个字段属于哪一类缺失。
//
// 取值用于补件清单：只有 "absent" 才可能通过"让对方补材料"解决；
// "unreadable" 需要重新提交更清晰的原件；
// "ambiguous" 需要人工核对。
func AbsenceReason(st FieldStatus) string {
	switch st {
	case Absent:
		return "absent"
	case Unreadable:
		return "unreadable"
	case Ambiguous:
		return "ambiguous"
	}
	return "unknown"
}

// 原件有无的取值，与抽取层一致。
const (
	PresencePresent    = "present"
	PresenceAbsent     = "absent"
	PresenceUnreadable = "unreadable"
)

// ClassifyFieldByPresence 用模型显式给出的 presence 判定字段状态。
// 这是首选路径：模型被要求直接回答"这一栏在原件上有没有"，
// 比拿置信度去猜"空值到底是没填还是看不清"可靠。
func ClassifyFieldByPresence(name FieldName, f Field, untrusted bool, threshold float64) (FieldStatus, bool) {
	if untrusted {
		return Ambiguous, true
	}
	switch f.Presence {
	case PresencePresent:
		if f.Value == "" {
			// 声称有值却是空：自相矛盾，按看不清处理。
			return Unreadable, true
		}
		if f.Conf < threshold {
			return Unreadable, true
		}
		return Present, true
	case PresenceAbsent:
		if f.Value != "" {
			// 声称没有却有值：以有值为准。
			return Present, false
		}
		return Absent, true
	case PresenceUnreadable:
		return Unreadable, true
	}
	return Unknown, false
}

// ClassifyField 根据抽取结果与两侧一致性，判断字段状态。
//
// 判定顺序（重要）：
//  1. 两侧值不一致 → Ambiguous（不管值是什么）
//  2. 两侧都为空、且都给"确认没有"的高置信度 → Absent
//  3. 两侧都为空、但有一侧置信度低 → Unreadable（看不清，不知道该不该信）
//  4. 有值但置信度低 → Unreadable
//  5. 有值且置信度高 → Present
//
// 第 2、3 条是这次改动的核心：同样是空值，置信度高就是"确认为空"，
// 置信度低就是"看不清"。用 confidence 表达这个区分，而不是用值本身。
func ClassifyField(name FieldName, a, b Field, hasA, hasB bool, untrusted bool, threshold float64) FieldStatus {
	if untrusted {
		return Ambiguous
	}
	if !hasA && !hasB {
		return Unknown
	}
	// 只有一侧有结果：以这一侧为准，但要求它自己足够确定。
	var only Field
	switch {
	case hasA && hasB:
		// 两侧都在，值已在上层比对过一致性；取较小置信度。
		only = Field{Value: a.Value, Conf: minf(a.Conf, b.Conf)}
	case hasA:
		only = a
	default:
		only = b
	}
	if only.Value == "" {
		// 空值：先看是不是"两侧都确认没有"。
		confidentBoth := hasA && hasB && a.Conf >= threshold && b.Conf >= threshold
		if confidentBoth {
			return Absent
		}
		// 只有一侧在，且它高置信度地给出空 → 也算确认没有（另一侧没跑成功时无从比对）。
		if !hasA || !hasB {
			if only.Conf >= threshold {
				return Absent
			}
			return Unreadable
		}
		return Unreadable
	}
	if only.Conf < threshold {
		return Unreadable
	}
	return Present
}

// Statuses 是字段状态的集合，便于整体传给结果与界面。
type Statuses map[FieldName]FieldStatus

// Of 返回某字段的状态（缺失时按 Unknown）。
func (s Statuses) Of(name FieldName) FieldStatus {
	if v, ok := s[name]; ok {
		return v
	}
	return Unknown
}

// MissingFacts 返回"确认原件没有"的字段（业务事实，不需要补件，但需要据此判定）。
func (s Statuses) MissingFacts() []FieldName {
	var out []FieldName
	for _, n := range FieldNames() {
		if s.Of(n) == Absent {
			out = append(out, n)
		}
	}
	return out
}

// NeedsEvidence 返回需要重新提供原件或人工确认的字段。
func (s Statuses) NeedsEvidence() []FieldName {
	var out []FieldName
	for _, n := range FieldNames() {
		if s.Of(n).NeedsEvidence() {
			out = append(out, n)
		}
	}
	return out
}

// Summary 是一句话说明，用于事件流与审核意见。
func (s Statuses) Summary() string {
	var absent, unread, ambig int
	for _, n := range FieldNames() {
		switch s.Of(n) {
		case Absent:
			absent++
		case Unreadable:
			unread++
		case Ambiguous:
			ambig++
		}
	}
	if absent == 0 && unread == 0 && ambig == 0 {
		return "全部字段已读到"
	}
	return fmt.Sprintf("原件未填写 %d 项，无法识别 %d 项，两模型不一致 %d 项", absent, unread, ambig)
}

func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
