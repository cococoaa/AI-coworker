package upload

import (
	"unicode/utf16"
	"unicode/utf8"
)

// PDF 字符串的解码。
//
// PDF 里的字符串没有统一编码声明：可能是 UTF-16BE（带 BOM）、
// 也可能是生成软件直接塞进去的 GBK。发票这种中文票据，前者最常见。
// 策略是"试到能读为止"，都读不出就原样返回——不猜、不做替换，
// 让 checkReadable 去判断这段字节到底是不是可读文字。

// decodeTextBytes 把 PDF 字符串字节还原成文本。
func decodeTextBytes(b []byte) string {
	if len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF {
		return decodeUTF16BE(b[2:])
	}
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		return decodeUTF16LE(b[2:])
	}
	// 无 BOM 但呈 UTF-16BE 特征：大量 0x00 在高字节位。
	// 无 BOM 的 UTF-16：靠"能否解出合理字符"判断，而不是靠高字节是否为 0。
	// 汉字的高字节非零，早先按"大量 0x00"判会漏掉整段中文。
	if s, ok := tryUTF16BE(b); ok {
		return s
	}
	if utf8.Valid(b) {
		return string(b)
	}
	// 不是合法 UTF-8，也不是 UTF-16：多半是单字节编码或字体子集编码。
	// 交给 checkReadable 判定；这里原样保留字节，不假装读懂。
	return string(b)
}

// tryUTF16BE 尝试按 UTF-16BE 解，只有"解出来确实可读"才采用。
//
// 判断标准是解码结果的可读性，不是字节分布：
// 中文文本的高字节普遍非零，按"大量 0x00"去找会把整段中文漏掉。
// 这里要求：解出的字符绝大多数是可读字符（不是控制符、不是私用区）。
func tryUTF16BE(b []byte) (string, bool) {
	if len(b) < 4 || len(b)%2 != 0 {
		return "", false
	}
	s := decodeUTF16BE(b)
	total, good := 0, 0
	for _, r := range s {
		total++
		if isReadableRune(r) {
			good++
		}
	}
	if total == 0 {
		return "", false
	}
	if float64(good)/float64(total) >= 0.95 {
		return s, true
	}
	return "", false
}

func decodeUTF16BE(b []byte) string {
	if len(b)%2 == 1 {
		b = b[:len(b)-1]
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
	}
	return string(utf16.Decode(u))
}

func decodeUTF16LE(b []byte) string {
	if len(b)%2 == 1 {
		b = b[:len(b)-1]
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i+1])<<8|uint16(b[i]))
	}
	return string(utf16.Decode(u))
}

// normalizeWhitespace 收拢空白：PDF 文本流里常有大量空隙与孤立换行。
// 连续空格压成一个、连续空行压成一个，行结构保留（下游按行解析）。
func normalizeWhitespace(s string) string {
	var out []byte
	prevSpace, prevNewline := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\r':
			continue
		case '\n':
			if !prevNewline && len(out) > 0 {
				out = append(out, '\n')
			}
			prevNewline, prevSpace = true, false
		case ' ', '\t':
			if !prevSpace && !prevNewline && len(out) > 0 {
				out = append(out, ' ')
			}
			prevSpace = true
		default:
			out = append(out, c)
			prevSpace, prevNewline = false, false
		}
	}
	return string(out)
}
