package task

import (
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetNow(func() time.Time { return time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC) })
	return s
}

func TestCreateStartsPending(t *testing.T) {
	s := newStore(t)
	tk, err := s.Create("C03", "BX26110003", "E1001")
	if err != nil {
		t.Fatal(err)
	}
	if tk.State != Pending {
		t.Fatalf("新建任务应为待审核，得到 %s", tk.State)
	}
	if tk.State.WaitingOn() != "等待开始审核" {
		t.Errorf("WaitingOn 应说明在等什么，得到 %q", tk.State.WaitingOn())
	}
	if len(tk.Events) != 1 || tk.Events[0].Kind != "created" {
		t.Errorf("应有受理事件: %+v", tk.Events)
	}
}

// 缺材料 → 补交 → 回到待审，这是闭环的主干。
func TestNeedMoreThenResubmit(t *testing.T) {
	s := newStore(t)
	tk, _ := s.Create("C03", "BX26110003", "E1001")

	// 审核给出"退回补材料"，列出缺件。
	missing := []MissingItem{
		{Key: "approval", Label: "出差审批单", Kind: "missing_attachment",
			Reason: "住宿报销须附出差审批单", Clause: "第3.1条"},
		{Key: "buyer_tax_id", Label: "购买方税号", Kind: "absent",
			Reason: "发票未填写购买方税号", Clause: "第2.2条"},
	}
	recheck := &Recheck{CheckIDs: []string{"R02-b", "R08-a"}, KeepVerified: []string{"verify_invoice"},
		Reason: "补件后只重查与缺件相关的判定项"}
	got, err := s.ApplyDecision(tk.ID, Run{Decision: "need_more_info", ApprovedFen: 0}, "need_more_info", missing, recheck, "E3001")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != NeedMore {
		t.Fatalf("应为待补材料，得到 %s", got.State)
	}
	if got.State.WaitingOn() != "报销人补交材料" {
		t.Errorf("应指明卡在报销人那里，得到 %q", got.State.WaitingOn())
	}
	if n := len(got.Outstanding()); n != 2 {
		t.Fatalf("应有 2 项待补，得到 %d", n)
	}
	if got.NextRecheck == nil || len(got.NextRecheck.CheckIDs) != 2 {
		t.Errorf("应记下续审要重查什么: %+v", got.NextRecheck)
	}

	// 补交第一项：还不齐，仍是待补。
	got, err = s.Resubmit(tk.ID, "approval", "审批单.pdf", "E1001", "补交审批单", "审批单正文")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != NeedMore {
		t.Fatalf("只补一项应仍待补，得到 %s", got.State)
	}
	if n := len(got.Outstanding()); n != 1 {
		t.Fatalf("应剩 1 项待补，得到 %d", n)
	}

	// 补交第二项：齐了，回到待审（等续审）。
	got, err = s.Resubmit(tk.ID, "buyer_tax_id", "发票重开.pdf", "E1001", "重新开票", "发票正文")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Pending {
		t.Fatalf("补齐后应回到待审，得到 %s", got.State)
	}
	if !got.ResolvedAll() {
		t.Error("应全部补齐")
	}
	// 材料版本：补交两次，各版本 1。
	if len(got.Attachments) != 2 {
		t.Fatalf("应有 2 个附件版本，得到 %d", len(got.Attachments))
	}
}

// 补交不在清单里的材料要报错，不能悄悄接受。
func TestResubmitUnknownItem(t *testing.T) {
	s := newStore(t)
	tk, _ := s.Create("C03", "BX1", "E1001")
	s.ApplyDecision(tk.ID, Run{}, "need_more_info",
		[]MissingItem{{Key: "approval", Label: "出差审批单", Kind: "missing_attachment"}}, nil, "E3001")

	if _, err := s.Resubmit(tk.ID, "something_else", "x.pdf", "E1001", "", "正文"); err == nil {
		t.Fatal("补交不在清单里的材料应报错")
	}
}

// 不是待补状态时拒绝补交。
func TestResubmitWrongState(t *testing.T) {
	s := newStore(t)
	tk, _ := s.Create("C01", "BX1", "E1001")
	s.ApplyDecision(tk.ID, Run{}, "approve", nil, nil, "E3001")
	if _, err := s.Resubmit(tk.ID, "approval", "x.pdf", "E1001", "", "正文"); err == nil {
		t.Fatal("已通过的任务不应接受补交")
	}
}

// 转人工：等财务，不是等员工。
func TestManualStateWaitsForFinance(t *testing.T) {
	s := newStore(t)
	tk, _ := s.Create("C51", "BX1", "E1001")
	got, _ := s.ApplyDecision(tk.ID, Run{}, "manual_review", nil, nil, "E3001")
	if got.State != NeedManual {
		t.Fatalf("应为待人工复核，得到 %s", got.State)
	}
	if got.State.WaitingOn() != "财务复核" {
		t.Errorf("应指明卡在财务，得到 %q", got.State.WaitingOn())
	}
}

// 同一单据已有待补任务时，不应重复创建（避免两张单互相覆盖）。
func TestNoDuplicateNeedMoreTask(t *testing.T) {
	s := newStore(t)
	tk, _ := s.Create("C03", "BX1", "E1001")
	s.ApplyDecision(tk.ID, Run{}, "need_more_info",
		[]MissingItem{{Key: "approval", Label: "审批单", Kind: "missing_attachment"}}, nil, "E3001")
	if _, err := s.Create("C03", "BX1", "E1001"); err == nil {
		t.Fatal("同一单据已有待补任务时不应重复创建")
	}
}

// 续审轮次累加，且每轮都留下记录。
func TestRoundsAccumulate(t *testing.T) {
	s := newStore(t)
	tk, _ := s.Create("C03", "BX1", "E1001")

	s.ApplyDecision(tk.ID, Run{Decision: "need_more_info"}, "need_more_info",
		[]MissingItem{{Key: "approval", Label: "审批单", Kind: "missing_attachment"}}, nil, "E3001")
	s.Resubmit(tk.ID, "approval", "a.pdf", "E1001", "", "正文")
	s.ApplyDecision(tk.ID, Run{Decision: "approve", ApprovedFen: 76000}, "approve", nil, nil, "E3001")

	got, _ := s.Get(tk.ID)
	if len(got.Runs) != 2 {
		t.Fatalf("应有 2 个审核批次，得到 %d", len(got.Runs))
	}
	if got.Runs[0].Round != 1 || got.Runs[1].Round != 2 {
		t.Errorf("轮次应递增: %+v", got.Runs)
	}
	if got.State != Approved {
		t.Errorf("最终应为通过，得到 %s", got.State)
	}
	if got.LastApprovedFen != 76000 {
		t.Errorf("核准金额应保留，得到 %d", got.LastApprovedFen)
	}
}

// 落盘：重启后仍知道每张单处理到哪一步。
func TestPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tasks.json")
	s1, _ := Open(p)
	s1.SetNow(func() time.Time { return time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC) })
	tk, _ := s1.Create("C03", "BX1", "E1001")
	s1.ApplyDecision(tk.ID, Run{}, "need_more_info",
		[]MissingItem{{Key: "approval", Label: "审批单", Kind: "missing_attachment"}}, nil, "E3001")

	// 重启
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Get(tk.ID)
	if !ok {
		t.Fatal("重启后任务应还在")
	}
	if got.State != NeedMore {
		t.Errorf("重启后状态应保留，得到 %s", got.State)
	}
	if len(got.Outstanding()) != 1 {
		t.Errorf("重启后补件清单应保留，得到 %d 项", len(got.Outstanding()))
	}
	// 序列号不撞车
	nt, _ := s2.Create("C04", "BX2", "E1002")
	if nt.ID == tk.ID {
		t.Error("重启后新建任务不应与旧任务同号")
	}
}

// 待办统计：财务最关心的"今天有多少要处理"。
func TestStats(t *testing.T) {
	s := newStore(t)
	for i, dec := range []string{"need_more_info", "manual_review", "approve", "manual_review"} {
		tk, _ := s.Create("C0"+string(rune('1'+i)), "BX", "E1")
		var missing []MissingItem
		if dec == "need_more_info" {
			missing = []MissingItem{{Key: "approval", Label: "审批单", Kind: "missing_attachment"}}
		}
		s.ApplyDecision(tk.ID, Run{}, dec, missing, nil, "E3001")
	}
	st := s.Stats()
	if st[NeedMore] != 1 || st[NeedManual] != 2 || st[Approved] != 1 {
		t.Errorf("统计错误: %v", st)
	}
}

// 撤销是幂等的。
func TestWithdrawIdempotent(t *testing.T) {
	s := newStore(t)
	tk, _ := s.Create("C01", "BX1", "E1001")
	a, err := s.Withdraw(tk.ID, "E1001", "重复提交")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Withdraw(tk.ID, "E1001", "重复提交")
	if err != nil {
		t.Fatalf("重复撤销应幂等，得到错误 %v", err)
	}
	if a.State != Withdrawn || b.State != Withdrawn {
		t.Error("撤销后状态应为 withdrawn")
	}
}

// 停在某个状态多久了——用于"卡了几天"的提醒。
func TestWaitingDays(t *testing.T) {
	s := newStore(t)
	tk, _ := s.Create("C03", "BX1", "E1001")
	s.ApplyDecision(tk.ID, Run{}, "need_more_info",
		[]MissingItem{{Key: "approval", Label: "审批单", Kind: "missing_attachment"}}, nil, "E3001")
	got, _ := s.Get(tk.ID)
	// 同一时刻应为 0 天
	if got.WaitingDays(time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)) != 0 {
		t.Error("刚变更应为 0 天")
	}
	if got.WaitingDays(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)) != 3 {
		t.Error("三天后应为 3 天")
	}
}
