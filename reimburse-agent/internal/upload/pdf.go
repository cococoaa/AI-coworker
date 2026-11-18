package upload

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// PDF 文本提取：只用标准库，且只在"确实读到了可读文字"时才成功。
//
// 第一版的做法是"把所有 stream 都解出来，能从里面捞出括号字符串就当文字"。
// 拿真实 PDF 一试就露馅了：字体的 TrueType 表、颜色配置（ICC）都是 stream，
// 里面同样有大量括号和字节序列，于是提取出 25 万个字符的乱码，
// 而且长度检查也通过了——"看起来成功"是最危险的那种失败。
//
// 现在改成两条硬要求：
//  1. **只认内容流**：必须有 /Contents 或内容流字典特征，字体/图片/元数据一律跳过；
//  2. **要求可读性**：提取结果里可打印字符与预期文字的比例必须达标，
//     汉字、数字、字母、常见标点之外的大量控制字符直接判定失败。
//
// 读不出来就返回错误。调用方据此按"原件看不清"处理，转人工。

// ErrNoTextLayer 表示 PDF 里没有可用的文本层（扫描件 / 图片型 / 加密 / 字体未嵌入）。
var ErrNoTextLayer = errors.New("该 PDF 没有可提取的文本层")

// ErrNotPDF 表示文件不是 PDF。
var ErrNotPDF = errors.New("不是有效的 PDF 文件")

// MinTextRunes 是可认为提取成功的最小字符数。
const MinTextRunes = 40

// minReadableRatio 是可读字符的最低占比。
//
// 真实电子发票的文本层几乎全是可读字符（汉字、数字、字母、标点），
// 偶尔有少量控制字符。占比低于 0.85 基本可以断定解出来的是二进制垃圾。
const minReadableRatio = 0.85

// ExtractPDFText 从 PDF 字节里提取文本。
func ExtractPDFText(b []byte) (string, error) {
	if !IsPDF(b) {
		return "", ErrNotPDF
	}
	if bytes.Contains(b, []byte("/Encrypt")) {
		return "", fmt.Errorf("%w（该 PDF 已加密，无法读取）", ErrNoTextLayer)
	}

	// 先收 CMap：Identity-H 编码的 PDF 必须靠它转码，
	// 否则内容流里的"字"只是字形编号，读出来是乱码。
	cm := collectCMaps(b)

	var out strings.Builder
	used := 0
	for _, s := range findStreams(b) {
		if !s.content {
			continue // 字体、图片、元数据：不是页面文字
		}
		data := s.data
		if s.flate {
			plain, err := inflate(data)
			if err != nil {
				continue
			}
			data = plain
		}
		text := textFromContent(data, cm)
		if strings.TrimSpace(text) == "" {
			continue
		}
		out.WriteString(text)
		used++
	}
	if used == 0 {
		return "", fmt.Errorf("%w（未找到内容流，可能是扫描件或图片型 PDF）", ErrNoTextLayer)
	}

	text := normalizeWhitespace(out.String())
	if err := checkReadable(text); err != nil {
		return "", err
	}
	return text, nil
}

// checkReadable 判定提取结果是否真的可读。
//
// 这一步是防"假成功"的闸门：宁可报错让人工录入，也不能把乱码当文本
// 送进模型——模型会从乱码里"抽出"看似合理的字段，那才是真正的幻觉源头。
func checkReadable(s string) error {
	rs := []rune(s)
	if len(rs) < MinTextRunes {
		return fmt.Errorf("%w（只提取到 %d 个字符，低于 %d；可能是扫描件）",
			ErrNoTextLayer, len(rs), MinTextRunes)
	}
	readable := 0
	for _, r := range rs {
		if isReadableRune(r) {
			readable++
		}
	}
	ratio := float64(readable) / float64(len(rs))
	if ratio < minReadableRatio {
		return fmt.Errorf("%w（可读字符占比 %.0f%%，低于 %.0f%%；"+
			"提取到的是字体或图像数据，不是页面文字）",
			ErrNoTextLayer, ratio*100, minReadableRatio*100)
	}
	return nil
}

// isReadableRune 判断一个字符是否算"可读"。
//
// 汉字、日文假名、韩文、字母、数字、空白与常见标点都算。
// 控制字符（除换行制表）、私用区、替换符不算。
func isReadableRune(r rune) bool {
	if r == '\n' || r == '\t' || r == ' ' {
		return true
	}
	if r < 0x20 || r == 0x7F {
		return false
	}
	if r >= 0xE000 && r <= 0xF8FF { // 私用区
		return false
	}
	if r == 0xFFFD { // 替换符
		return false
	}
	if unicode.IsGraphic(r) || unicode.IsSpace(r) {
		return true
	}
	return utf8.ValidRune(r)
}

// pdfStream 是一段 stream 内容。
type pdfStream struct {
	data  []byte
	flate bool
	// content 为真表示这是页面内容流（可能含文字），否则是字体/图片/元数据。
	content bool
}

// findStreams 扫描 stream...endstream 段，并判断它是不是内容流。
func findStreams(b []byte) []pdfStream {
	var out []pdfStream
	const kw = "stream"
	i := 0
	for {
		k := bytes.Index(b[i:], []byte(kw))
		if k < 0 {
			break
		}
		start := i + k
		if start >= 3 && bytes.Equal(b[start-3:start], []byte("end")) {
			i = start + len(kw)
			continue
		}
		p := start + len(kw)
		if p < len(b) && b[p] == '\r' {
			p++
		}
		if p < len(b) && b[p] == '\n' {
			p++
		}
		end := bytes.Index(b[p:], []byte("endstream"))
		if end < 0 {
			break
		}
		data := b[p : p+end]

		// 字典在 stream 之前。
		dictStart := bytes.LastIndex(b[max(0, start-4096):start], []byte("<<"))
		dict := ""
		if dictStart >= 0 {
			dict = string(b[max(0, start-4096)+dictStart : start])
		}

		out = append(out, pdfStream{
			data:    bytes.TrimRight(data, "\r\n"),
			flate:   strings.Contains(dict, "FlateDecode"),
			content: looksLikeContentStream(dict),
		})
		i = p + end + len("endstream")
	}
	return out
}

// looksLikeCMapStream 判断这段流（可能压缩）是不是 ToUnicode CMap。
func looksLikeCMapStream(data []byte) bool {
	probe := data
	if len(probe) < 4 || probe[0] != 0x78 {
		// 不是 zlib 头就直观察看。
		return bytes.Contains(data, []byte("begincmap")) ||
			bytes.Contains(data, []byte("beginbfchar")) ||
			bytes.Contains(data, []byte("beginbfrange"))
	}
	if plain, err := inflate(data); err == nil {
		probe = plain
	}
	return bytes.Contains(probe, []byte("begincmap")) ||
		bytes.Contains(probe, []byte("beginbfchar")) ||
		bytes.Contains(probe, []byte("beginbfrange"))
}

// looksLikeContentStream 判断这个 stream 是不是页面内容流。
//
// 排除法为主：字体（/FontFile、/Subtype/Type1、/TrueType、/CIDFont）、
// 图片（/Image、/DCTDecode、/JPXDecode）、元数据（/Metadata、/XML）、
// 颜色配置（/ICCBased）、交叉引用流（/XRef）都不是文字来源。
// 剩下的、或明确标了 /Contents 的，才当作内容流。
func looksLikeContentStream(dict string) bool {
	exclude := []string{
		"/FontFile", "/FontFile2", "/FontFile3",
		"/Subtype/Image", "/Subtype /Image", "/Image",
		"/DCTDecode", "/JPXDecode", "/JBIG2Decode", "/CCITTFaxDecode",
		"/Metadata", "/XML", "/ICCBased", "/XRef", "/ObjStm",
		"/Type/Font", "/Type /Font", "/Type/EmbeddedFile",
		"/Length1", "/Length2", // TrueType 字体表
		"/ToUnicode", "/CIDToGIDMap",
	}
	for _, e := range exclude {
		if strings.Contains(dict, e) {
			return false
		}
	}
	// 明确的内容流标记。
	if strings.Contains(dict, "/Contents") {
		return true
	}
	// 只声明了长度（和压缩）的匿名流，通常就是页面内容。
	trimmed := strings.TrimSpace(dict)
	return strings.HasPrefix(trimmed, "<<") &&
		(!strings.Contains(trimmed, "/Type") || strings.Contains(trimmed, "/Page"))
}

// inflate 解 zlib 流。
func inflate(b []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(io.LimitReader(r, 16<<20))
}

// textFromContent 从内容流里抽出文字。
//
// 处理 Tj / TJ / ' / " 四种文本操作符，按 Td/TD/T*/Tm 断行。
// 不做字体映射（ToUnicode CMap）：那需要解析字体子集，工作量与收益不成正比。
// 代价是：如果 PDF 用自定义编码且没有 ToUnicode，提取出的会是乱码——
// 这种情况由 checkReadable 拦住，转人工，而不是硬猜。
func textFromContent(b []byte, cm *cmap) string {
	var out strings.Builder
	var cur strings.Builder

	flushLine := func() {
		line := strings.TrimSpace(cur.String())
		if line != "" {
			out.WriteString(line)
			out.WriteString("\n")
		}
		cur.Reset()
	}

	i := 0
	for i < len(b) {
		switch b[i] {
		case '(':
			raw, n := readLiteralRaw(b[i:])
			if n == 0 {
				i++
				continue
			}
			cur.WriteString(decodeWithCMap(raw, cm))
			i += n
			continue
		case '<':
			if i+1 < len(b) && b[i+1] == '<' {
				i += 2
				continue
			}
			raw, n := readHexRaw(b[i:])
			cur.WriteString(decodeWithCMap(raw, cm))
			i += n
			continue
		}
		if hasOp(b[i:], "Td") || hasOp(b[i:], "TD") || hasOp(b[i:], "T*") || hasOp(b[i:], "Tm") {
			flushLine()
			i += 2
			continue
		}
		if hasOp(b[i:], "Tj") || hasOp(b[i:], "TJ") {
			i += 2
			continue
		}
		i++
	}
	flushLine()
	return out.String()
}

// hasOp 判断当前位置是否以某个操作符开头，且后一个字符不是字母（避免误匹配 Tjx）。
func hasOp(b []byte, op string) bool {
	if len(b) < len(op) || string(b[:len(op)]) != op {
		return false
	}
	if len(b) > len(op) {
		c := b[len(op)]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			return false
		}
	}
	return true
}

// decodeWithCMap 用 CMap 把编码字节转成文字；没有 CMap 时按常规编码猜。
//
// 判定顺序很重要：先试 CMap，因为 Identity-H 编码的 PDF 只有 CMap 能解对。
// CMap 里查不到该编码时，退回按 UTF-16/UTF-8 解——有些 PDF 直接存明文。
func decodeWithCMap(raw []byte, cm *cmap) string {
	if len(raw) == 0 {
		return ""
	}
	if cm != nil && len(cm.table) > 0 {
		if s, ok := cmapLookup(raw, cm); ok {
			return s
		}
	}
	return decodeTextBytes(raw)
}

// cmapLookup 按编码宽度切分字节并查表。
//
// 一段字符串里可能有多个字符（Identity-H 下每 2 字节一个字），
// 逐个查表拼接；任何一个查不到就整体放弃，退回常规解码——
// 部分成功更危险：会拼出"看起来像"但实际错位的文字。
func cmapLookup(raw []byte, cm *cmap) (string, bool) {
	w := cm.width
	if w < 1 {
		w = 2
	}
	if len(raw)%w != 0 {
		return "", false
	}
	var sb strings.Builder
	for i := 0; i+w <= len(raw); i += w {
		key := strings.ToUpper(hexKey(raw[i : i+w]))
		v, ok := cm.table[key]
		if !ok {
			return "", false
		}
		sb.WriteString(v)
	}
	if sb.Len() == 0 {
		return "", false
	}
	return sb.String(), true
}

// hexKey 把字节转成十六进制键。
func hexKey(b []byte) string {
	const hexdigits = "0123456789ABCDEF"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hexdigits[c>>4], hexdigits[c&0x0F])
	}
	return string(out)
}

// readLiteralRaw reads the raw bytes inside (...) with escapes resolved.
func readLiteralRaw(b []byte) ([]byte, int) {
	if len(b) == 0 || b[0] != '(' {
		return nil, 0
	}
	var out []byte
	depth := 1
	i := 1
	for i < len(b) && depth > 0 {
		c := b[i]
		if c == '\\' {
			if i+1 >= len(b) {
				i++
				continue
			}
			n := b[i+1]
			switch n {
			case 'n':
				out = append(out, 10)
			case 'r':
				out = append(out, 13)
			case 't':
				out = append(out, 9)
			case 'b', 'f':
			case '(', ')', '\\':
				out = append(out, n)
			case 13:
				if i+2 < len(b) && b[i+2] == 10 {
					i++
				}
			case 10:
			default:
				if n >= '0' && n <= '7' {
					v := int(n - '0')
					j := i + 2
					for k := 0; k < 2 && j < len(b) && b[j] >= '0' && b[j] <= '7'; k++ {
						v = v*8 + int(b[j]-'0')
						j++
					}
					out = append(out, byte(v))
					i = j
					continue
				}
				out = append(out, n)
			}
			i += 2
			continue
		}
		if c == '(' {
			depth++
			out = append(out, c)
			i++
			continue
		}
		if c == ')' {
			depth--
			if depth == 0 {
				i++
				return out, i
			}
			out = append(out, c)
			i++
			continue
		}
		out = append(out, c)
		i++
	}
	return out, i
}

// readHexRaw reads the raw bytes of a <...> hex string.
func readHexRaw(b []byte) ([]byte, int) {
	if len(b) == 0 || b[0] != '<' {
		return nil, 0
	}
	end := bytes.IndexByte(b, '>')
	if end < 0 {
		return nil, len(b)
	}
	h := strings.Map(func(r rune) rune {
		if r == ' ' || r == 10 || r == 13 || r == 9 {
			return -1
		}
		return r
	}, string(b[1:end]))
	if len(h)%2 == 1 {
		h += "0"
	}
	out := make([]byte, 0, len(h)/2)
	for i := 0; i+1 < len(h); i += 2 {
		out = append(out, byte(hexVal(h[i])*16+hexVal(h[i+1])))
	}
	return out, end + 1
}

// readLiteralString 读 PDF 的 (...) 字符串，处理转义。
func readLiteralString(b []byte) (string, int) {
	if len(b) == 0 || b[0] != '(' {
		return "", 0
	}
	var out []byte
	depth := 1
	i := 1
	for i < len(b) && depth > 0 {
		c := b[i]
		switch c {
		case '\\':
			if i+1 >= len(b) {
				i++
				continue
			}
			n := b[i+1]
			switch n {
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'b', 'f':
			case '(', ')', '\\':
				out = append(out, n)
			case '\r':
				if i+2 < len(b) && b[i+2] == '\n' {
					i++
				}
			case '\n':
			default:
				if n >= '0' && n <= '7' {
					v := int(n - '0')
					j := i + 2
					for k := 0; k < 2 && j < len(b) && b[j] >= '0' && b[j] <= '7'; k++ {
						v = v*8 + int(b[j]-'0')
						j++
					}
					out = append(out, byte(v))
					i = j
					continue
				}
				out = append(out, n)
			}
			i += 2
			continue
		case '(':
			depth++
			out = append(out, c)
		case ')':
			depth--
			if depth == 0 {
				i++
				return decodeTextBytes(out), i
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
		i++
	}
	return decodeTextBytes(out), i
}

// readHexString 读 <...> 十六进制串。
func readHexString(b []byte) (string, int) {
	if len(b) == 0 || b[0] != '<' {
		return "", 0
	}
	end := bytes.IndexByte(b, '>')
	if end < 0 {
		return "", len(b)
	}
	hex := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, string(b[1:end]))
	if len(hex)%2 == 1 {
		hex += "0"
	}
	out := make([]byte, 0, len(hex)/2)
	for i := 0; i+1 < len(hex); i += 2 {
		out = append(out, byte(hexVal(hex[i])*16+hexVal(hex[i+1])))
	}
	return decodeTextBytes(out), end + 1
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return 0
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
