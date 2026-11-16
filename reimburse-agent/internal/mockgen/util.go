package mockgen

import (
	"fmt"
	"math/rand"
	"strings"
	"time"
)

const dateLayout = "2006-01-02"

func mustDate(s string) time.Time {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		panic(fmt.Sprintf("日期格式错误: %q", s))
	}
	return t
}

// addDays 对 YYYY-MM-DD 加减天数。
func addDays(s string, n int) string { return mustDate(s).AddDate(0, 0, n).Format(dateLayout) }

// daysBetween 返回 b 减 a 的天数。
func daysBetween(a, b string) int { return int(mustDate(b).Sub(mustDate(a)).Hours() / 24) }

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func setOf(ss []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// yuanStr 把分格式化为 "553.00"。
func yuanStr(fen int64) string {
	sign := ""
	if fen < 0 {
		sign, fen = "-", -fen
	}
	return fmt.Sprintf("%s%d.%02d", sign, fen/100, fen%100)
}

// group4 把 0-9999 转成大写数字（不带万、亿单位）。
func group4(g int64) string {
	const digits = "零壹贰叁肆伍陆柒捌玖"
	units := []string{"", "拾", "佰", "仟"}
	d := []rune(digits)
	var out string
	pendingZero := false
	for pos := 3; pos >= 0; pos-- {
		v := (g / pow10(pos)) % 10
		if v == 0 {
			pendingZero = true
			continue
		}
		if pendingZero && out != "" {
			out += "零"
		}
		out += string(d[v]) + units[pos]
		pendingZero = false
	}
	return out
}

func pow10(n int) int64 {
	r := int64(1)
	for i := 0; i < n; i++ {
		r *= 10
	}
	return r
}

// daxie 把分转成发票上的中文大写金额。
func daxie(fen int64) string {
	if fen == 0 {
		return "零元整"
	}
	d := []rune("零壹贰叁肆伍陆柒捌玖")
	yuan, jiao, fe := fen/100, (fen/10)%10, fen%10
	big := []string{"", "万", "亿"}
	var s strings.Builder
	if yuan > 0 {
		var groups []int64
		for y := yuan; y > 0; y /= 10000 {
			groups = append(groups, y%10000)
		}
		zeroPending := false
		for gi := len(groups) - 1; gi >= 0; gi-- {
			g := groups[gi]
			if g == 0 {
				zeroPending = true
				continue
			}
			if zeroPending || (g < 1000 && gi != len(groups)-1) {
				s.WriteString("零")
			}
			s.WriteString(group4(g) + big[gi])
			zeroPending = false
		}
		s.WriteString("元")
	}
	switch {
	case jiao == 0 && fe == 0:
		s.WriteString("整")
	default:
		if jiao > 0 {
			s.WriteString(string(d[jiao]) + "角")
		} else if yuan > 0 {
			s.WriteString("零")
		}
		if fe > 0 {
			s.WriteString(string(d[fe]) + "分")
		}
	}
	return s.String()
}

func pick[T any](r *rand.Rand, xs []T) T { return xs[r.Intn(len(xs))] }

func shuffled[T any](r *rand.Rand, xs []T) []T {
	out := append([]T(nil), xs...)
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func equalSets(a, b []string) bool {
	sa, sb := setOf(a), setOf(b)
	if len(sa) != len(sb) {
		return false
	}
	for k := range sa {
		if !sb[k] {
			return false
		}
	}
	return true
}
