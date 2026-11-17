package judge

// 平台侧与登记侧的固定取值。宿主负责把自己的枚举翻译成这些值；
// 插件内部只认这几个字符串，避免和宿主的 schema 包耦合。

const (
	invNormal     = "normal"
	invVoided     = "voided"
	invRedReverse = "red_reversed"

	coActive   = "存续"
	coCanceled = "注销"
	coRevoked  = "吊销"
	coAbnormal = "经营异常"

	histApproved  = "approved"
	histReviewing = "reviewing"
	histRejected  = "rejected"
)

// containsField 判断 FieldName 切片是否包含某值。
func containsField(xs []FieldName, x FieldName) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// appendField 去重追加 FieldName。
func appendField(xs []FieldName, x FieldName) []FieldName {
	if containsField(xs, x) {
		return xs
	}
	return append(xs, x)
}

// appendStr 去重追加 string。
func appendStr(xs []string, x string) []string {
	for _, s := range xs {
		if s == x {
			return xs
		}
	}
	return append(xs, x)
}

// containsStr 判断字符串切片是否包含某值。
func containsStr(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// CategoryCN 返回报销类别的中文名，供界面与审核意见使用。
func CategoryCN(c string) string {
	switch c {
	case "transport":
		return "交通"
	case "lodging":
		return "住宿"
	case "meal":
		return "餐饮"
	}
	return c
}

// ToolCN 返回核验状态的中文名。
func ToolCN(s Status) string {
	switch s {
	case Found:
		return "查到"
	case NotFound:
		return "查无"
	case Failed:
		return "失败或未调用"
	case Timeout:
		return "超时"
	}
	return "未知"
}
