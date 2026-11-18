package upload

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ToUnicode CMap 解析。
//
// 为什么必须有它：中文电子发票（以及 Chrome/Skia 导出的 PDF）几乎都用
// Identity-H 编码——内容流里的"字"只是字体子集里的字形编号，不是 Unicode。
// 不查 CMap 直接读，得到的就是一串十六进制乱码。
//
// 第一版没做这一步，拿真实 PDF 一试：25 万字符全是垃圾。
// 加可读性闸门后能拦住，但那些 PDF 明明是文本型的，一律拒绝也不对。
//
// CMap 的关键结构：
//
//	beginbfchar  <3D> <4FE1>           单个映射
//	endbfchar
//	beginbfrange <00> <FF> <4E00>      区间映射（起点、终点、起始 Unicode）
//	endbfrange
//
// 只解析这两类，覆盖绝大多数情况。CIDFont 的复杂 CMap 不处理——
// 那需要完整的 PostScript 解释，收益远小于成本。

// cmap 是从"字符编码"到"Unicode 文本"的映射。
type cmap struct {
	// table 的键是编码字节串（通常 1–2 字节）转成的字符串。
	table map[string]string
	// width 是编码宽度（由 codespacerange 推断，常见 2 字节）。
	width int
}

// parseCMap 从 CMap 流里解析映射表。
func parseCMap(data []byte) *cmap {
	c := &cmap{table: map[string]string{}, width: 2}
	s := string(data)

	// codespacerange 决定编码宽度：<00> <FF> 是 1 字节，<0000> <FFFF> 是 2 字节。
	if i := strings.Index(s, "begincodespacerange"); i >= 0 {
		if j := strings.Index(s[i:], "endcodespacerange"); j > 0 {
			seg := s[i : i+j]
			if k := strings.Index(seg, "<"); k >= 0 {
				if e := strings.Index(seg[k:], ">"); e > 0 {
					c.width = (e - 1) / 2
					if c.width < 1 {
						c.width = 1
					}
				}
			}
		}
	}

	parseBFChar(c, s)
	parseBFRange(c, s)
	return c
}

// parseBFChar 解析 beginbfchar ... endbfchar。
func parseBFChar(c *cmap, s string) {
	for {
		i := strings.Index(s, "beginbfchar")
		if i < 0 {
			return
		}
		j := strings.Index(s[i:], "endbfchar")
		if j < 0 {
			return
		}
		seg := s[i+len("beginbfchar") : i+j]
		// 每行形如 <src> <dst>：按行切，不能把整段的所有 <...> 平铺成一对对，
		// 否则多行时会错位。
		for _, line := range strings.Split(seg, "\n") {
			pairs := hexPairs(line)
			if len(pairs) < 2 {
				continue
			}
			c.table[strings.ToUpper(pairs[0])] = decodeCMapTarget(pairs[1])
		}
		s = s[i+j+len("endbfchar"):]
	}
}

// parseBFRange 解析 beginbfrange ... endbfrange。
func parseBFRange(c *cmap, s string) {
	for {
		i := strings.Index(s, "beginbfrange")
		if i < 0 {
			return
		}
		j := strings.Index(s[i:], "endbfrange")
		if j < 0 {
			return
		}
		seg := s[i+len("beginbfrange") : i+j]
		s = s[i+j+len("endbfrange"):]

		// 每行形如 <lo> <hi> <dst>，dst 也可能是 [<a> <b> ...]。
		for _, line := range strings.Split(seg, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "<") {
				continue
			}
			pairs := hexPairs(line)
			if len(pairs) < 2 {
				continue
			}
			lo, ok1 := hexToInt(pairs[0])
			hi, ok2 := hexToInt(pairs[1])
			if !ok1 || !ok2 || hi < lo || hi-lo > 65535 {
				continue
			}
			// 目标在数组里：逐项对应。
			if k := strings.Index(line, "["); k >= 0 {
				if e := strings.Index(line[k:], "]"); e > 0 {
					arr := hexPairs(line[k : k+e])
					for n, a := range arr {
						if lo+n > hi {
							break
						}
						c.table[intToHexKey(lo+n, len(pairs[0])/2)] = decodeCMapTarget(a)
					}
					continue
				}
			}
			if len(pairs) < 3 {
				continue
			}
			base, ok3 := hexToInt(pairs[2])
			if !ok3 {
				continue
			}
			for code := lo; code <= hi; code++ {
				c.table[intToHexKey(code, len(pairs[0])/2)] = string(rune(base + (code - lo)))
			}
		}
	}
}

// hexPairs 抽出一行里所有的 <...> 十六进制串。
func hexPairs(s string) []string {
	var out []string
	for i := 0; i < len(s); i++ {
		if s[i] != '<' {
			continue
		}
		e := strings.IndexByte(s[i:], '>')
		if e < 0 {
			break
		}
		v := strings.Map(func(r rune) rune {
			if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
				return -1
			}
			return r
		}, s[i+1:i+e])
		if v != "" {
			out = append(out, strings.ToUpper(v))
		}
		i += e
	}
	return out
}

// decodeCMapTarget 把 CMap 的目标（Unicode 十六进制）还原成文字。
// 目标可能是多字节（比如一个字形对应两个字），按 UTF-16BE 解。
func decodeCMapTarget(hex string) string {
	if len(hex)%2 == 1 {
		hex += "0"
	}
	b := make([]byte, 0, len(hex)/2)
	for i := 0; i+1 < len(hex); i += 2 {
		v := hexVal(hex[i])*16 + hexVal(hex[i+1])
		b = append(b, byte(v))
	}
	if len(b) >= 2 {
		return decodeUTF16BE(b)
	}
	if len(b) == 1 {
		return string(rune(b[0]))
	}
	return ""
}

func hexToInt(h string) (int, bool) {
	if h == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(h, 16, 32)
	if err != nil {
		return 0, false
	}
	return int(v), true
}

// intToHexKey 把编码转成定宽的十六进制键，和内容流里的写法一致。
func intToHexKey(v, width int) string {
	if width <= 0 {
		width = 2
	}
	return fmt.Sprintf("%0*X", width*2, v)
}

// 从 PDF 里把 CMap 找出来。
//
// 做法：把每个 stream 都解压，看里面有没有 beginbfchar / beginbfrange。
// 这类流体积小（几百字节到几十 KB），成本可以接受。
// 找到的映射合并成一张总表——多字体的编码可能重叠，但发票通常只有一两种字体，
// 冲突时后写的覆盖先写的，实践中够用。
func collectCMaps(b []byte) *cmap {
	total := &cmap{table: map[string]string{}, width: 2}
	for _, s := range findStreams(b) {
		// 不看 content 标记：判 CMap 只认内容特征。
		// 早先按"内容流里不会有 CMap"跳过，结果所有 CMap 都被误判成内容流，
		// 表是空的，提取出来全是乱码。
		data := s.data
		if s.flate {
			plain, err := inflate(data)
			if err != nil {
				continue
			}
			data = plain
		}
		if !bytes.Contains(data, []byte("beginbfchar")) && !bytes.Contains(data, []byte("beginbfrange")) {
			continue
		}
		one := parseCMap(data)
		for k, v := range one.table {
			total.table[k] = v
		}
		if one.width > total.width {
			total.width = one.width
		}
	}
	return total
}

var _ = zlib.NewReader
var _ = io.ReadAll
