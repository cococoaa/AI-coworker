package judge

import (
	"fmt"
	"strconv"
	"strings"
)

// normalize 只做格式归一化，不消除数值差异：
//   - 金额补两位小数（"553" 与 "553.00" 视为同一金额）
//   - 税号与发票号去空格、转大写
//   - 日期去空格
//
// 刻意不做的事情：四舍五入到角、模糊匹配、纠错。这些会让"两个模型看到不同数字"
// 这种真实分歧被吞掉。
func normalize(name FieldName, v string) string {
	t := strings.TrimSpace(v)
	switch name {
	case FTotalAmount:
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return t
		}
		return fmt.Sprintf("%.2f", f)
	case FBuyerTaxID, FSellerTaxID, FInvoiceNo:
		return strings.ToUpper(t)
	}
	return t
}
