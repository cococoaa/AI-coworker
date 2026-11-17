package policy

import "strings"

// 统一社会信用代码（GB 32100-2015）：18 位，前 17 位加权求和后取校验位。
const taxIDCharset = "0123456789ABCDEFGHJKLMNPQRTUWXY"

var taxIDWeights = [17]int{1, 3, 9, 27, 19, 26, 16, 17, 20, 29, 25, 13, 8, 24, 10, 30, 28}

// TaxIDCharset 返回统一社会信用代码允许使用的字符集。
func TaxIDCharset() string { return taxIDCharset }

// TaxIDCheckChar 根据前 17 位计算校验位。前 17 位含非法字符或长度不对时 ok 为 false。
func TaxIDCheckChar(first17 string) (c byte, ok bool) {
	if len(first17) != 17 {
		return 0, false
	}
	sum := 0
	for i := 0; i < 17; i++ {
		idx := strings.IndexByte(taxIDCharset, first17[i])
		if idx < 0 {
			return 0, false
		}
		sum += idx * taxIDWeights[i]
	}
	return taxIDCharset[(31-sum%31)%31], true
}

// ValidTaxID 判断是否为位数、字符集、校验位都合法的统一社会信用代码。
func ValidTaxID(id string) bool {
	if len(id) != 18 {
		return false
	}
	c, ok := TaxIDCheckChar(id[:17])
	return ok && c == id[17]
}
