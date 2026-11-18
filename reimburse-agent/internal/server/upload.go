package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"reimburse/internal/schema"
	"reimburse/internal/upload"
)

// 真实票据上传。
//
// 两个入口：
//   POST /api/upload         multipart/form-data（浏览器用）
//   POST /api/upload/json    JSON + base64（脚本用，不依赖 multipart）
//
// 上传成功后返回单据编号；这个单号可以直接送给 /api/audit 审核，
// 也就是说上传不是"另存一份文件"，而是"造出一条能进现有链路的单据"。
//
// 几个刻意的取舍：
//   - 提取失败就失败：不返回半份文本让模型去猜。理由写在 upload 包里。
//   - 佐证凭证失败只警告：缺件由规则层按"缺材料"处理，不影响主判定。
//   - 不写 truth.json：上传的票据没有标准答案，HasTruth=false，
//     合成演示模式会因此拒绝它（这是刻意的，不是 bug）。

const maxUploadBytes = 12 << 20 // 12 MB：发票 8 MB + 凭证 4 MB，留点表单开销

// uploadResponse 是上传成功的返回。
type uploadResponse struct {
	OK       bool   `json:"ok"`
	CaseID   string `json:"case_id"`
	Title    string `json:"title"`
	Category string `json:"category"`
	// InvoiceNo 是识别出的发票号码，界面直接展示给用户确认。
	InvoiceNo string `json:"invoice_no,omitempty"`
	// InvoiceChars 是提取到的字符数，便于判断提取质量。
	InvoiceChars int `json:"invoice_chars"`
	// HasSupport 表示佐证凭证是否成功提取。
	HasSupport bool     `json:"has_support"`
	Warnings   []string `json:"warnings,omitempty"`
	// Next 告诉调用方下一步能做什么。
	Next string `json:"next"`
}

// handleUpload 接收 multipart 上传。
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonOut(w, map[string]any{"error": "只支持 POST"}, 405)
		return
	}
	if s.Uploader == nil {
		jsonOut(w, map[string]any{"error": "未启用上传（服务启动时未配置上传目录）"}, 501)
		return
	}

	// 限制请求体大小：r.Body 被包装后，超限会在读的时候报错。
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		jsonOut(w, map[string]any{
			"ok":    false,
			"error": "解析上传内容失败：" + err.Error(),
			"hint":  "单次上传不超过 12 MB；发票 PDF 建议 8 MB 以内",
		}, 400)
		return
	}

	req, err := s.uploadRequestFromForm(r)
	if err != nil {
		jsonOut(w, map[string]any{"ok": false, "error": err.Error()}, 400)
		return
	}
	s.doUpload(w, req)
}

// uploadRequestFromForm 从表单里取报销单与两个文件。
func (s *Server) uploadRequestFromForm(r *http.Request) (upload.Request, error) {
	var req upload.Request
	get := func(k string) string { return strings.TrimSpace(r.FormValue(k)) }

	req.Claim = schema.Claim{
		ClaimNo:    get("claim_no"),
		RequestNo:  get("request_no"),
		Category:   get("category"),
		FromCity:   get("from_city"),
		City:       get("city"),
		StartDate:  get("start_date"),
		EndDate:    get("end_date"),
		Purpose:    get("purpose"),
		SubmitDate: get("submit_date"),
	}
	req.Claim.Applicant.Name = get("applicant_name")
	req.Claim.Applicant.EmployeeID = get("applicant_id")
	req.Claim.Applicant.Grade = get("applicant_grade")
	req.Claim.ExternalOrg = get("external_org")

	if v := get("amount_fen"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return req, fmt.Errorf("金额格式不正确：%q（应为整数分）", v)
		}
		req.Claim.AmountFen = n
	}
	if v := get("internal_count"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			req.Claim.InternalCount = n
		}
	}
	if v := get("external_count"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			req.Claim.ExternalCount = n
		}
	}

	inv, invName, err := readUploadPart(r, "invoice")
	if err != nil {
		return req, err
	}
	req.InvoiceData, req.InvoiceName = inv, invName

	// 佐证凭证可选。
	if sup, supName, err := readUploadPartOptional(r, "support"); err == nil && len(sup) > 0 {
		req.SupportData, req.SupportName = sup, supName
	}

	if err := validateClaimBasics(req.Claim); err != nil {
		return req, err
	}
	return req, nil
}

// readUploadPart 读一个必填文件。
func readUploadPart(r *http.Request, field string) ([]byte, string, error) {
	f, hdr, err := r.FormFile(field)
	if err != nil {
		return nil, "", fmt.Errorf("缺少文件：%s（表单字段名 %q）", fieldLabel(field), field)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, upload.MaxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("读取 %s 失败: %w", fieldLabel(field), err)
	}
	return b, hdr.Filename, nil
}

// readUploadPartOptional 读一个可选文件，缺失时不报错。
func readUploadPartOptional(r *http.Request, field string) ([]byte, string, error) {
	f, hdr, err := r.FormFile(field)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, upload.MaxSupportBytes+1))
	if err != nil {
		return nil, "", err
	}
	return b, hdr.Filename, nil
}

func fieldLabel(field string) string {
	if field == "invoice" {
		return "发票文件"
	}
	return "佐证凭证"
}

// validateClaimBasics 校验报销单的必填项。
//
// 只校验"没有它就没法审"的字段：类别、申请人、金额（金额为 0 时允许，
// 以发票为准——真实场景里员工常把金额填错，那正是要审出来的）。
func validateClaimBasics(c schema.Claim) error {
	switch c.Category {
	case "transport", "lodging", "meal":
	case "":
		return fmt.Errorf("请选择报销类别（交通 / 住宿 / 餐饮）")
	default:
		return fmt.Errorf("报销类别取值不合法：%q", c.Category)
	}
	if c.Applicant.EmployeeID == "" {
		return fmt.Errorf("请填写申请人工号")
	}
	return nil
}

// doUpload 执行上传并返回结果。
func (s *Server) doUpload(w http.ResponseWriter, req upload.Request) {
	res, err := s.Uploader.Save(req)
	if err != nil {
		// 提取失败要分类告诉用户该怎么办，而不是一句"上传失败"。
		code, action := 400, ""
		msg := err.Error()
		switch {
		case strings.Contains(msg, "文本层"), strings.Contains(msg, "扫描件"),
			strings.Contains(msg, "OCR"), strings.Contains(msg, "加密"):
			// 这类问题用户改不了，只能换文件或转人工。
			action = "请换一份电子发票（带文字层的 PDF），或转人工录入；扫描件需要 OCR，当前版本不支持"
		case strings.Contains(msg, "超过"), strings.Contains(msg, "上限"):
			code = 413
			action = "请压缩或裁剪后再上传"
		case strings.Contains(msg, "发票号码"):
			action = "上传的文件里没有可识别的发票号码；请确认上传的是发票而不是行程单或凭证"
		}
		jsonOut(w, map[string]any{"ok": false, "error": msg, "action": action}, code)
		return
	}

	c := res.Case
	// 让新单据进入内存，后续 /api/audit 能按编号找到它。
	s.AddCase(c)

	invNo := ""
	if id := extractInvoiceNo(c.InvoiceText); id != "" {
		invNo = id
	}
	out := uploadResponse{
		OK: true, CaseID: c.ID,
		Title:        "上传票据 " + c.Claim.ClaimNo,
		Category:     c.Claim.Category,
		InvoiceNo:    invNo,
		InvoiceChars: len([]rune(c.InvoiceText)),
		HasSupport:   c.SupportText != "",
		Warnings:     res.Warnings,
		Next: fmt.Sprintf("可以开始审核：POST /api/audit，case_id=%s。"+
			"注意：上传的票据没有标准答案，-demo-synthetic 模式下不适用，需配置模型密钥", c.ID),
	}
	jsonOut(w, out, 200)
}

// uploadJSONRequest 是 JSON 上传的请求体（文件用 base64）。
type uploadJSONRequest struct {
	Claim       schema.Claim `json:"claim"`
	InvoiceName string       `json:"invoice_name"`
	InvoiceB64  string       `json:"invoice_base64"`
	SupportName string       `json:"support_name"`
	SupportB64  string       `json:"support_base64"`
}

// handleUploadJSON 接收 JSON 上传，便于脚本与命令行使用。
func (s *Server) handleUploadJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonOut(w, map[string]any{"error": "只支持 POST"}, 405)
		return
	}
	if s.Uploader == nil {
		jsonOut(w, map[string]any{"error": "未启用上传"}, 501)
		return
	}
	var req uploadJSONRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxUploadBytes*2)).Decode(&req); err != nil {
		jsonOut(w, map[string]any{"ok": false, "error": "请求体解析失败：" + err.Error()}, 400)
		return
	}
	inv, err := decodeB64(req.InvoiceB64)
	if err != nil {
		jsonOut(w, map[string]any{"ok": false, "error": "发票 base64 解码失败"}, 400)
		return
	}
	var sup []byte
	if req.SupportB64 != "" {
		sup, _ = decodeB64(req.SupportB64)
	}
	if err := validateClaimBasics(req.Claim); err != nil {
		jsonOut(w, map[string]any{"ok": false, "error": err.Error()}, 400)
		return
	}
	s.doUpload(w, upload.Request{
		Claim: req.Claim, InvoiceName: req.InvoiceName, InvoiceData: inv,
		SupportName: req.SupportName, SupportData: sup,
	})
}

// decodeB64 解 base64，容忍有无填充与换行。
func decodeB64(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	return base64.StdEncoding.DecodeString(s)
}

// extractInvoiceNo 从发票文本里取号码，只用于展示与查重建档。
func extractInvoiceNo(text string) string {
	return upload.FindInvoiceNo(text)
}
