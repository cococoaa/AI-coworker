package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// 复核结论必须落盘：它改变了查重库，"这张票报过"不能在重启后消失。
func TestReviewPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reviews.json")

	s := testServer(t)
	if err := s.OpenReviewStore(path); err != nil {
		t.Fatal(err)
	}
	h := s.Routes()
	body, _ := json.Marshal(map[string]any{"case_id": "C01", "claim_no": "BX26110001",
		"decision": "approve", "note": "票据齐全", "employee_id": "E3001"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("POST", "/api/review", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("复核失败: %d %s", rec.Code, rec.Body.String())
	}
	if len(s.extra()) != 1 {
		t.Fatalf("应回写 1 条查重记录，得到 %d", len(s.extra()))
	}

	// 模拟重启：全新实例，只挂同一份文件。
	s2 := testServer(t)
	if err := s2.OpenReviewStore(path); err != nil {
		t.Fatal(err)
	}
	if n := len(s2.Reviews()); n != 1 {
		t.Fatalf("重启后复核记录应还在，实际 %d 条", n)
	}
	if n := len(s2.extra()); n != 1 {
		t.Fatalf("重启后查重库回写应还在，实际 %d 条", n)
	}
	// 编号序列也要接着走，不能重启后从 RV0001 重来（会撞车，撤销就撤错）。
	rv := s2.Reviews()[0]
	if rv.ID != "RV0001" {
		t.Errorf("编号应保留，得到 %s", rv.ID)
	}
	body, _ = json.Marshal(map[string]any{"case_id": "C02", "decision": "approve", "employee_id": "E3001"})
	rec = httptest.NewRecorder()
	s2.Routes().ServeHTTP(rec, localReq("POST", "/api/review", bytes.NewReader(body)))
	var resp struct {
		Review ReviewOutcome `json:"review"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Review.ID != "RV0002" {
		t.Errorf("重启后新记录应为 RV0002，得到 %s", resp.Review.ID)
	}
}

// 已撤销的记录也要留在盘上：审计要看"谁批的、谁撤的、为什么撤"。
func TestRevokedReviewPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reviews.json")

	s := testServer(t)
	if err := s.OpenReviewStore(path); err != nil {
		t.Fatal(err)
	}
	h := s.Routes()
	body, _ := json.Marshal(map[string]any{"case_id": "C01", "decision": "approve", "employee_id": "E3001"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("POST", "/api/review", bytes.NewReader(body)))
	var resp struct {
		Review ReviewOutcome `json:"review"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	body, _ = json.Marshal(map[string]any{"id": resp.Review.ID, "reason": "重复报销", "employee_id": "E3001"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("POST", "/api/review/revoke", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("撤销失败: %d", rec.Code)
	}

	s2 := testServer(t)
	if err := s2.OpenReviewStore(path); err != nil {
		t.Fatal(err)
	}
	rs := s2.Reviews()
	if len(rs) != 1 {
		t.Fatalf("撤销记录应保留，实际 %d 条", len(rs))
	}
	if !rs[0].Revoked() {
		t.Error("重启后仍应标记为已撤销")
	}
	if rs[0].RevokeReason != "重复报销" {
		t.Errorf("撤销原因应保留，得到 %q", rs[0].RevokeReason)
	}
	if n := len(s2.extra()); n != 0 {
		t.Errorf("已撤销的结论不应再影响查重库，实际 %d 条", n)
	}
}

// 重复提交同一条复核结论：不新增记录、不重复回写查重库。
func TestReviewIsIdempotent(t *testing.T) {
	s := testServer(t)
	h := s.Routes()
	payload := map[string]any{"case_id": "C01", "claim_no": "BX26110001",
		"decision": "approve", "note": "票据齐全", "employee_id": "E3001"}

	var first ReviewOutcome
	for i := 0; i < 3; i++ {
		body, _ := json.Marshal(payload)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, localReq("POST", "/api/review", bytes.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("第 %d 次提交失败: %d %s", i+1, rec.Code, rec.Body.String())
		}
		var resp struct {
			Review    ReviewOutcome `json:"review"`
			Duplicate bool          `json:"duplicate"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if i == 0 {
			if resp.Duplicate {
				t.Error("首次提交不应判为重复")
			}
			first = resp.Review
			continue
		}
		if !resp.Duplicate {
			t.Errorf("第 %d 次应判为重复", i+1)
		}
		if resp.Review.ID != first.ID {
			t.Errorf("重复提交应返回同一条记录，得到 %s 与 %s", resp.Review.ID, first.ID)
		}
	}
	if n := len(s.Reviews()); n != 1 {
		t.Errorf("三次相同提交只应留下 1 条记录，实际 %d", n)
	}
	if n := len(s.extra()); n != 1 {
		t.Errorf("查重库只应写入 1 条，实际 %d", n)
	}

	// 意见不同就是另一条结论：复核人改了判断，不能吞掉。
	payload2 := map[string]any{"case_id": "C01", "claim_no": "BX26110001",
		"decision": "approve", "note": "改为按票面金额", "employee_id": "E3001"}
	body, _ := json.Marshal(payload2)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("POST", "/api/review", bytes.NewReader(body)))
	var resp struct {
		Review    ReviewOutcome `json:"review"`
		Duplicate bool          `json:"duplicate"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Duplicate {
		t.Error("意见不同应视为新结论")
	}
	if n := len(s.Reviews()); n != 2 {
		t.Errorf("应新增 1 条记录，实际共 %d 条", n)
	}
}

// 撤销后再提交同样结论：应重新生效（新记录），不能被判成"已存在"。
func TestRevokedThenResubmitSame(t *testing.T) {
	s := testServer(t)
	h := s.Routes()
	payload := map[string]any{"case_id": "C01", "decision": "approve", "employee_id": "E3001"}

	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("POST", "/api/review", bytes.NewReader(body)))
	var r1 struct {
		Review ReviewOutcome `json:"review"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &r1)

	body, _ = json.Marshal(map[string]any{"id": r1.Review.ID, "reason": "撤错", "employee_id": "E3001"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("POST", "/api/review/revoke", bytes.NewReader(body)))

	body, _ = json.Marshal(payload)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("POST", "/api/review", bytes.NewReader(body)))
	var r2 struct {
		Review    ReviewOutcome `json:"review"`
		Duplicate bool          `json:"duplicate"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &r2)
	if r2.Duplicate {
		t.Error("已撤销的结论不应拦住同样的重新提交")
	}
	if r2.Review.ID == r1.Review.ID {
		t.Error("重新提交应生成新记录，而不是复活已撤销的那条")
	}
	if n := len(s.extra()); n != 1 {
		t.Errorf("重新提交应恢复查重库记录，实际 %d 条", n)
	}
}
