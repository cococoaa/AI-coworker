package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// 上传表单的布局约束：按钮不能被表单顶出视口。
//
// 这是一个真实踩过的坑：12 个字段单列堆叠时表单高约 960px，
// 而常见笔记本视口高度只有 800–900px。「上传并建单」按钮落在 y=960，
// 用户要滚动才能看到；自动化工具更是直接报"element outside of the viewport"。
//
// 约束固定在样式里，不靠人记：
//   - 表单用两列栅格（高度减半）；
//   - 按钮放在吸底的行里（滚动时始终可见）；
//   - 左栏面板允许内部滚动（长表单可达）。
//
// 这类问题只能在真浏览器里量，所以这里检查样式的关键规则是否还在；
// 真正的视口验证在 docs 的验收步骤里用浏览器量。

func TestUploadFormLayoutConstraints(t *testing.T) {
	page := pageSource(t)

	checks := []struct {
		name string
		need string
		why  string
	}{
		{
			"表单两列",
			"grid-template-columns:1fr 1fr",
			"单列 12 个字段会把按钮顶到视口外",
		},
		{
			"按钮行吸底",
			"position:sticky;bottom:0",
			"按钮要在滚动时始终可见，不能随表单滚走",
		},
		{
			"左栏可滚动",
			".panel.left #uploadPane{overflow:auto",
			"面板 overflow:hidden 时，超出部分永远点不到",
		},
		{
			"整行元素跨列",
			".upform .full{grid-column:1 / -1}",
			"提示与文件选择要占满整行，否则挤在两列里变形",
		},
	}
	for _, c := range checks {
		if !strings.Contains(page, c.need) {
			t.Errorf("%s：样式里缺少 %q\n原因：%s", c.name, c.need, c.why)
		}
	}
}

// 上传按钮与结果区必须存在且 id 与脚本一致：
// 少一个 id，点击就没反应——这种错在界面上完全静默。
func TestUploadFormElements(t *testing.T) {
	page := pageSource(t)
	ids := []string{
		"tabUpload", "uploadPane", "upBtn", "upSample", "upStatus", "upResult",
		"up_invoice", "up_support", "up_request_no", "up_applicant_id", "up_category", "upMore", "upPreview",
	}
	for _, id := range ids {
		if !strings.Contains(page, `id="`+id+`"`) {
			t.Errorf("缺少元素 id=%q（脚本会找不到它）", id)
		}
	}
	// 脚本必须真的绑定了上传动作与样例按钮。
	for _, fn := range []string{"uploadInvoice", "useSampleInvoice"} {
		if !strings.Contains(page, "function "+fn) {
			t.Errorf("缺少函数 %s", fn)
		}
	}
	if !strings.Contains(page, `$("#upBtn").onclick = uploadInvoice`) {
		t.Error("上传按钮没有绑定点击处理")
	}
	if !strings.Contains(page, `$("#upSample").onclick = useSampleInvoice`) {
		t.Error("样例按钮没有绑定点击处理")
	}
}

// 样例发票端点必须可用：现场没有合规票据时靠它验证链路。
func TestSampleInvoiceEndpoint(t *testing.T) {
	s := testServer(t)
	rec := httptestRecorder()
	s.Routes().ServeHTTP(rec, localReq("GET", "/api/sample-invoice", nil))

	if rec.Code == 200 {
		if ct := rec.Header().Get("Content-Type"); ct != "application/pdf" {
			t.Errorf("样例应为 PDF，得到 %q", ct)
		}
		if rec.Body.Len() < 100 {
			t.Errorf("样例内容过小：%d 字节", rec.Body.Len())
		}
		return
	}
	// 测试环境里没有 samples 目录时返回 404 并说明原因，不能 500。
	if rec.Code != 404 {
		t.Errorf("缺少样例时应 404，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "样例") {
		t.Errorf("404 也应说明原因：%s", rec.Body.String())
	}
}

// indexHTML 取出嵌入的页面源码。
func pageSource(t *testing.T) string {
	t.Helper()
	return string(PageHTML())
}

// httptestRecorder 是 httptest.NewRecorder 的短名。
func httptestRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }
