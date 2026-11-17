package server

import (
	"testing"

	"reimburse/internal/schema"
	"reimburse/internal/task"
)

// 补交材料必须真的写进单据。
//
// 这是"假闭环"最容易发生的地方：补交接口返回成功、状态变成待审核，
// 但续审时用的还是原始单据，结论一点没变。用户会觉得"我补了你怎么还说缺"。
// 下面几条逐项验证：补什么，单据上就改什么。
func TestApplySupplementsSupport(t *testing.T) {
	c := mustCase(t, "C44")
	tk := &task.Task{Attachments: []task.Attachment{
		{Kind: "support", Content: "行程单正文：2026-10-12 上海虹桥→北京南 G102 553.00", Version: 1},
	}}
	eff, unknown := applySupplements(c, tk)
	if eff.SupportText != "行程单正文：2026-10-12 上海虹桥→北京南 G102 553.00" {
		t.Errorf("佐证凭证应被替换，得到 %q", eff.SupportText)
	}
	if len(unknown) != 0 {
		t.Errorf("support 是已知类型，不应报未识别: %v", unknown)
	}
	// 原单据不能被改：续审可以跑多次，改坏原件就再也回不去了。
	if c.SupportText != "" {
		t.Error("不应修改原始用例")
	}
}

// 补申请单号：报销单上的 RequestNo 必须真的变。
func TestApplySupplementsRequestNo(t *testing.T) {
	c := mustCase(t, "C45")
	if c.Claim.RequestNo != "" {
		t.Skipf("C45 本来就有申请单号: %q", c.Claim.RequestNo)
	}
	tk := &task.Task{Attachments: []task.Attachment{
		{Kind: "claim_info", Content: "申请单号：SQ20261008001", Version: 1},
	}}
	eff, unknown := applySupplements(c, tk)
	if eff.Claim.RequestNo != "SQ20261008001" {
		t.Errorf("申请单号应写入报销单，得到 %q", eff.Claim.RequestNo)
	}
	if len(unknown) != 0 {
		t.Errorf("中文键名应被识别: %v", unknown)
	}
}

// 一次补多项：key=value 逐行给。
func TestApplySupplementsMultiple(t *testing.T) {
	c := mustCase(t, "C46")
	tk := &task.Task{Attachments: []task.Attachment{
		{Kind: "claim_info", Content: "external_org=启明建设集团\ninternal_count=3", Version: 1},
	}}
	eff, unknown := applySupplements(c, tk)
	if eff.Claim.ExternalOrg != "启明建设集团" {
		t.Errorf("招待对象单位应写入，得到 %q", eff.Claim.ExternalOrg)
	}
	if eff.Claim.InternalCount != 3 {
		t.Errorf("内部人数应写入，得到 %d", eff.Claim.InternalCount)
	}
	if len(unknown) != 0 {
		t.Errorf("不应有未识别键: %v", unknown)
	}
}

// 认不出的键要报告出来，不能悄悄丢掉。
func TestApplySupplementsReportsUnknown(t *testing.T) {
	c := mustCase(t, "C45")
	tk := &task.Task{Attachments: []task.Attachment{
		{Kind: "claim_info", Content: "报销单编号=BX999\nfoo=bar", Version: 1},
	}}
	_, unknown := applySupplements(c, tk)
	if len(unknown) == 0 {
		t.Fatal("不认识的键必须报出来，否则用户以为补上了")
	}
	if len(unknown) != 2 {
		t.Errorf("应报 2 个未识别键，得到 %v", unknown)
	}
}

// 最新版本覆盖旧版本：补交两次应以第二次为准。
func TestApplySupplementsLatestWins(t *testing.T) {
	c := mustCase(t, "C45")
	tk := &task.Task{Attachments: []task.Attachment{
		{Kind: "claim_info", Content: "申请单号：SQ-OLD", Version: 1},
		{Kind: "claim_info", Content: "申请单号：SQ-NEW", Version: 2},
	}}
	eff, _ := applySupplements(c, tk)
	if eff.Claim.RequestNo != "SQ-NEW" {
		t.Errorf("应以最新一版为准，得到 %q", eff.Claim.RequestNo)
	}
}

// 空正文的补交不生效（接口层已拦，这里兜底）。
func TestApplySupplementsIgnoresEmpty(t *testing.T) {
	c := mustCase(t, "C45")
	tk := &task.Task{Attachments: []task.Attachment{
		{Kind: "claim_info", Content: "   ", Version: 1},
	}}
	eff, unknown := applySupplements(c, tk)
	if eff.Claim.RequestNo != c.Claim.RequestNo {
		t.Error("空补交不应改动单据")
	}
	if len(unknown) != 0 {
		t.Errorf("空补交不应报未识别: %v", unknown)
	}
}

func mustCase(t *testing.T, id string) *schema.Case {
	t.Helper()
	c, ok := testServer(t).Find(id)
	if !ok {
		t.Skipf("用例 %s 不可用", id)
	}
	return c
}
