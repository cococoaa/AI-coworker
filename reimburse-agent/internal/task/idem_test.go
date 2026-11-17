package task

import "testing"

// 补交的幂等：同一份材料提交两次，只应留一个版本。
//
// 这条不是洁癖。真实场景里双击、断网重试、报销人没看到回执又点一次都会重复提交。
// 如果每次都加一个版本，"版本历史"就变成了操作记录，财务看不出到底补交了几次。
func TestResubmitIsIdempotent(t *testing.T) {
	s := newStore(t)
	tk, _ := s.Create("C44", "BX26110044", "E1001")
	s.ApplyDecision(tk.ID, Run{}, "need_more_info", []MissingItem{
		{Key: "support", Label: "行程单", Kind: "missing_attachment", Reason: "缺少行程单"},
	}, nil, "E3001")

	got, dup, err := s.ResubmitIdem(tk.ID, "support", "行程单.txt", "E1001", "补交", "行程单正文")
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Fatal("第一次提交不应判为重复")
	}
	if len(got.Attachments) != 1 || got.Attachments[0].Version != 1 {
		t.Fatalf("首次补交应留下版本 1，得到 %+v", got.Attachments)
	}

	// 第二次：同样的内容。
	got, dup, err = s.ResubmitIdem(tk.ID, "support", "行程单.txt", "E1001", "补交", "行程单正文")
	if err != nil {
		t.Fatalf("重复提交应成功而不是报错，得到 %v", err)
	}
	if !dup {
		t.Error("同样内容重复提交应标记为重复")
	}
	if len(got.Attachments) != 1 {
		t.Errorf("重复提交不应新增附件版本，实际 %d 个", len(got.Attachments))
	}
	if got.Attachments[0].Version != 1 {
		t.Errorf("重复提交不应推进版本号，实际 %d", got.Attachments[0].Version)
	}

	// 第三次：内容不同，是真正的补正，应新增版本 2。
	got, dup, err = s.ResubmitIdem(tk.ID, "support", "行程单-改.txt", "E1001", "重新提供", "行程单正文（清晰版）")
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Error("内容不同不应判为重复")
	}
	if len(got.Attachments) != 2 {
		t.Fatalf("新内容应新增版本，实际 %d 个", len(got.Attachments))
	}
	if got.Attachments[1].Version != 2 {
		t.Errorf("补正应为版本 2，实际 %d", got.Attachments[1].Version)
	}
	// 续审必须用最新版，而不是第一次的。
	if txt, ok := got.LatestContent("support"); !ok || txt != "行程单正文（清晰版）" {
		t.Errorf("最新内容应取版本 2，得到 %q", txt)
	}
}

// 空白差异不算"新内容"：前后多了换行还是同一份材料。
func TestResubmitIgnoresWhitespace(t *testing.T) {
	s := newStore(t)
	tk, _ := s.Create("C44", "BX26110045", "E1001")
	s.ApplyDecision(tk.ID, Run{}, "need_more_info", []MissingItem{
		{Key: "support", Label: "行程单", Kind: "missing_attachment"},
	}, nil, "E3001")

	if _, _, err := s.ResubmitIdem(tk.ID, "support", "a.txt", "E1001", "", "正文"); err != nil {
		t.Fatal(err)
	}
	got, dup, err := s.ResubmitIdem(tk.ID, "support", "b.txt", "E1001", "", "\n  正文 \n")
	if err != nil {
		t.Fatal(err)
	}
	if !dup || len(got.Attachments) != 1 {
		t.Errorf("只有空白差异应视为同一份材料，dup=%v 附件数=%d", dup, len(got.Attachments))
	}
}
