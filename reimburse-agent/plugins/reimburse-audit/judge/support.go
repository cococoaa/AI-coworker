package judge

import (
	"regexp"
	"strconv"
	"strings"
)

// 佐证凭证（行程单、水单、消费清单）的字段解析。
//
// 为什么不交给模型：佐证凭证只有少量固定字段，用确定性规则解析更快、更便宜，
// 而且解析不出来时可以明确标为"无从判定"，不会让模型猜一个席位或姓名出来。
// 发票字段（金额、税号、发票号）仍由模型抽取并交叉校验。

// SupportDoc 是解析后的佐证凭证。
type SupportDoc struct {
	Present  bool
	Kind     string // itinerary / folio / consumption / unknown
	Mode     string // train / flight
	Person   string
	Date     string
	CheckIn  string
	CheckOut string
	Nights   int
	Seat     string
	Lines    []string // 品名，用于禁止项目检查
	TotalFen int64
	HasTotal bool
}

var (
	rePerson   = regexp.MustCompile(`(?:乘车人|旅客姓名|入住人)\s*[:：]\s*(\S+)`)
	reDate     = regexp.MustCompile(`(?:乘车日期|乘机日期|消费日期)\s*[:：]\s*(\d{4}-\d{2}-\d{2})`)
	reCheckIn  = regexp.MustCompile(`入住日期\s*[:：]\s*(\d{4}-\d{2}-\d{2})`)
	reCheckOut = regexp.MustCompile(`离店日期\s*[:：]\s*(\d{4}-\d{2}-\d{2})`)
	reNights   = regexp.MustCompile(`共\s*(\d+)\s*晚`)
	reSeat     = regexp.MustCompile(`(?:席别|舱位)\s*[:：]\s*(\S+)`)
	reTotal    = regexp.MustCompile(`合计\s*[:：]\s*[¥￥]?\s*([\d,]+\.\d{2})`)
	reLineAmt  = regexp.MustCompile(`^(.+?)\s*(?:x\d+\s+)?[¥￥]\s*[\d,]+\.\d{2}$`)
)

// ParseSupport 解析佐证凭证文本。文本为空时 Present 为 false。
func ParseSupport(text string) SupportDoc {
	d := SupportDoc{Present: strings.TrimSpace(text) != ""}
	if !d.Present {
		return d
	}
	head := text
	if i := strings.Index(text, "\n"); i > 0 {
		head = text[:i]
	}
	switch {
	case strings.Contains(head, "客票行程单"), strings.Contains(head, "铁路"), strings.Contains(head, "车次"):
		d.Kind = "itinerary"
	case strings.Contains(head, "水单"), strings.Contains(head, "结账"):
		d.Kind = "folio"
	case strings.Contains(head, "消费清单"):
		d.Kind = "consumption"
	default:
		d.Kind = "unknown"
	}
	if d.Kind == "itinerary" {
		d.Mode = "train"
		if strings.Contains(text, "航班") || strings.Contains(text, "乘机") {
			d.Mode = "flight"
		}
	}
	if m := rePerson.FindStringSubmatch(text); m != nil {
		d.Person = strings.TrimSpace(m[1])
	}
	if m := reDate.FindStringSubmatch(text); m != nil {
		d.Date = m[1]
	}
	if m := reCheckIn.FindStringSubmatch(text); m != nil {
		d.CheckIn = m[1]
	}
	if m := reCheckOut.FindStringSubmatch(text); m != nil {
		d.CheckOut = m[1]
	}
	if m := reNights.FindStringSubmatch(text); m != nil {
		d.Nights, _ = strconv.Atoi(m[1])
	}
	if m := reSeat.FindStringSubmatch(text); m != nil {
		d.Seat = strings.TrimSpace(m[1])
	}
	if m := reTotal.FindStringSubmatch(text); m != nil {
		if v, err := parseYuan(m[1]); err == nil {
			d.TotalFen, d.HasTotal = v, true
		}
	}
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.Contains(ln, "合计") || strings.Contains(ln, "品名") {
			continue
		}
		if m := reLineAmt.FindStringSubmatch(ln); m != nil {
			name := strings.TrimSpace(m[1])
			// 水单的逐日行以日期开头，取名称部分。
			if len(name) > 10 && name[:4] >= "2020" && name[:4] <= "2030" {
				name = strings.TrimSpace(name[10:])
			}
			name = strings.TrimRight(strings.TrimSpace(name), ":：")
			if name != "" {
				d.Lines = append(d.Lines, name)
			}
		}
	}
	return d
}

// 引擎内部用一组小函数从 Input 里取佐证凭证，避免把 SupportDoc 暴露到公开 API。

func (in *Input) support() SupportDoc {
	if in.supportDoc == nil {
		d := ParseSupport(in.SupportText)
		in.supportDoc = &d
	}
	return *in.supportDoc
}

func supportRendered(in *Input) (SupportDoc, bool) {
	s := in.support()
	return s, s.Present
}

func supportTotal(in *Input) (int64, bool) {
	s := in.support()
	return s.TotalFen, s.HasTotal
}

func supportDate(in *Input, key string) (string, bool) {
	s := in.support()
	var v string
	switch key {
	case "date":
		v = s.Date
	case "check_in":
		v = s.CheckIn
	case "check_out":
		v = s.CheckOut
	}
	return v, v != ""
}

func supportField(in *Input, key string) (string, bool) {
	s := in.support()
	if key == "seat" {
		return s.Seat, s.Seat != ""
	}
	if key == "person" {
		return s.Person, s.Person != ""
	}
	return "", false
}

func supportInt(in *Input, key string) (int, bool) {
	s := in.support()
	if key == "nights" {
		return s.Nights, s.Nights > 0
	}
	return 0, false
}

func supportMode(in *Input) string {
	s := in.support()
	if s.Mode == "" {
		return "train"
	}
	return s.Mode
}
