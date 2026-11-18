package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"

	"reimburse/internal/upload"
)

// 上传接口的端到端测试。
//
// 重点不是"能不能传上去"，而是几条边界：
//   - 成功时单据必须能直接送审（进内存、可查、有标题）；
//   - 失败时必须明确说清原因与下一步，不能返回一个空单据让下游去猜；
//   - 上传的票据没有标准答案，不能带出 expected 字段（那会误导对照）。

func newUploadServer(t *testing.T) *Server {
	t.Helper()
	s := testServer(t)
	s.Uploader = &upload.Processor{Root: t.TempDir()}
	return s
}

// multipartBody 组装一次上传请求体。
func multipartBody(t *testing.T, fields map[string]string, fileName string, fileData []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if fileName != "" {
		fw, err := w.CreateFormFile("invoice", fileName)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write(fileData)
	}
	w.Close()
	return &buf, w.FormDataContentType()
}

func baseFields() map[string]string {
	return map[string]string{
		"claim_no": "BX26110999", "request_no": "SQ20261118001", "category": "transport",
		"applicant_id": "E1001", "applicant_name": "张伟",
		"from_city": "上海", "city": "杭州",
		"start_date": "2026-11-10", "end_date": "2026-11-11",
		"purpose": "客户现场支持", "amount_fen": "7300",
	}
}

const invoiceText = "电子发票（普通发票）\n发票号码：26157293145980000777\n" +
	"开票日期：2026年11月10日\n购买方名称：星河科技有限公司\n" +
	"销售方名称：东海铁路客运服务有限公司\n价税合计（小写）¥73.00\n"

// 上传成功后：单据能查、有标题、没有标准答案、能直接送审。
func TestUploadCreatesAuditableCase(t *testing.T) {
	s := newUploadServer(t)
	h := s.Routes()

	body, ct := multipartBody(t, baseFields(), "invoice.txt", []byte(invoiceText))
	rec := httptest.NewRecorder()
	req := localReq("POST", "/api/upload", body)
	req.Header.Set("Content-Type", ct)
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("上传失败: %d %s", rec.Code, rec.Body.String())
	}
	var up struct {
		OK           bool   `json:"ok"`
		CaseID       string `json:"case_id"`
		InvoiceNo    string `json:"invoice_no"`
		InvoiceChars int    `json:"invoice_chars"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	if !up.OK || up.CaseID == "" {
		t.Fatalf("应返回单号: %+v", up)
	}
	if up.InvoiceNo != "26157293145980000777" {
		t.Errorf("发票号应被识别，得到 %q", up.InvoiceNo)
	}

	// 单据必须进了内存，且能查到。
	c, ok := s.Find(up.CaseID)
	if !ok {
		t.Fatal("上传的单据应能被 Find 到，否则无法审核")
	}
	if c.HasTruth {
		t.Error("上传票据不应有标准答案")
	}

	// 详情接口要有标题、不能带 expected。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("GET", "/api/cases/"+up.CaseID, nil))
	var detail map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	if detail["title"] == "" || detail["title"] == nil {
		t.Error("上传单据必须有标题，不能是空白")
	}
	if _, has := detail["expected"]; has {
		t.Error("没有标准答案时不应返回 expected，否则界面对照会误导")
	}
	if detail["uploaded"] != true {
		t.Error("应标记为上传的单据")
	}
}

// 提取失败时：报错 + 给出下一步，不能返回半份文本。
func TestUploadFailsWithoutText(t *testing.T) {
	s := newUploadServer(t)
	body, ct := multipartBody(t, baseFields(), "scan.pdf",
		[]byte("%PDF-1.4\n1 0 obj\n<< /Length 8 >>\nstream\n\x00\x01\x02\x03\nendstream\nendobj\n"))
	rec := httptest.NewRecorder()
	req := localReq("POST", "/api/upload", body)
	req.Header.Set("Content-Type", ct)
	s.Routes().ServeHTTP(rec, req)

	if rec.Code == 200 {
		t.Fatal("没有文本层时必须失败")
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["error"] == nil || resp["error"] == "" {
		t.Error("必须说明失败原因")
	}
	if resp["action"] == nil || resp["action"] == "" {
		t.Error("必须告诉用户下一步该做什么")
	}
	// 失败不能留下任何单据。
	if len(s.Cases) != 60 {
		t.Errorf("失败不应新增单据，当前 %d 条（测试集 60）", len(s.Cases))
	}
}

// 上传的不是发票（没有发票号码）时拒绝：否则会拿一份凭证当发票去审。
func TestUploadRejectsNonInvoice(t *testing.T) {
	s := newUploadServer(t)
	body, ct := multipartBody(t, baseFields(), "itinerary.txt",
		[]byte("行程单\n乘车人：张伟\n2026-11-10 上海到杭州\n票价 73 元\n"))
	rec := httptest.NewRecorder()
	req := localReq("POST", "/api/upload", body)
	req.Header.Set("Content-Type", ct)
	s.Routes().ServeHTTP(rec, req)

	if rec.Code == 200 {
		t.Fatal("没有发票号码的文本不应被接受")
	}
	if !strings.Contains(rec.Body.String(), "发票号码") {
		t.Errorf("错误应指出缺少发票号码: %s", rec.Body.String())
	}
}

// 图片票据：明确说需要 OCR，而不是含糊地"上传失败"。
func TestUploadRejectsImageWithOcrHint(t *testing.T) {
	s := newUploadServer(t)
	body, ct := multipartBody(t, baseFields(), "invoice.jpg", []byte{0xFF, 0xD8, 0xFF, 0xE0})
	rec := httptest.NewRecorder()
	req := localReq("POST", "/api/upload", body)
	req.Header.Set("Content-Type", ct)
	s.Routes().ServeHTTP(rec, req)

	if rec.Code == 200 {
		t.Fatal("图片应被拒绝")
	}
	body2 := rec.Body.String()
	if !strings.Contains(body2, "OCR") {
		t.Errorf("应说明需要 OCR: %s", body2)
	}
}

// 缺类别/缺工号这类表单错误要早拦，并说清缺什么。
func TestUploadValidatesForm(t *testing.T) {
	s := newUploadServer(t)
	cases := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{"缺类别", func(f map[string]string) { delete(f, "category") }, "类别"},
		{"类别非法", func(f map[string]string) { f["category"] = "gift" }, "类别"},
		{"缺工号", func(f map[string]string) { delete(f, "applicant_id") }, "工号"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := baseFields()
			c.mutate(f)
			body, ct := multipartBody(t, f, "invoice.txt", []byte(invoiceText))
			rec := httptest.NewRecorder()
			req := localReq("POST", "/api/upload", body)
			req.Header.Set("Content-Type", ct)
			s.Routes().ServeHTTP(rec, req)
			if rec.Code == 200 {
				t.Fatalf("%s 应被拒绝", c.name)
			}
			if !strings.Contains(rec.Body.String(), c.want) {
				t.Errorf("错误应提到 %q: %s", c.want, rec.Body.String())
			}
		})
	}
}

// 没有配置 Uploader 时返回 501，而不是崩溃。
func TestUploadDisabled(t *testing.T) {
	s := testServer(t) // 不设 Uploader
	body, ct := multipartBody(t, baseFields(), "invoice.txt", []byte(invoiceText))
	rec := httptest.NewRecorder()
	req := localReq("POST", "/api/upload", body)
	req.Header.Set("Content-Type", ct)
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != 501 {
		t.Errorf("未启用上传应返回 501，得到 %d", rec.Code)
	}
}

// 佐证凭证用图片：整体成功，但要有警告说明"按未提供处理"。
func TestUploadSupportImageWarns(t *testing.T) {
	s := newUploadServer(t)
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range baseFields() {
		w.WriteField(k, v)
	}
	fw, _ := w.CreateFormFile("invoice", "invoice.txt")
	fw.Write([]byte(invoiceText))
	sw, _ := w.CreateFormFile("support", "scan.jpg")
	sw.Write([]byte{0xFF, 0xD8, 0xFF})
	w.Close()

	rec := httptest.NewRecorder()
	req := localReq("POST", "/api/upload", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("凭证失败不应影响整体: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		HasSupport bool     `json:"has_support"`
		Warnings   []string `json:"warnings"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.HasSupport {
		t.Error("凭证提取失败时 has_support 应为 false")
	}
	if len(resp.Warnings) == 0 {
		t.Error("凭证失败必须给警告，不能静默")
	}
}
