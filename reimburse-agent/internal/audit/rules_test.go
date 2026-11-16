package audit

import (
	"testing"

	"reimburse/rules"
)

// 规则表自身的门禁：
//   - 每条规则的每个判定项都必须被引擎显式处理（要么可能命中，要么会被记入 Skipped），
//     不允许出现"规则表里有、代码里忘了"的静默漏判；
//   - 每个判定项都要能给出依据文字（Reason），不能只给编号；
//   - 结论必须按 decision_priority 取最严格者。
func TestEveryCheckIsHandled(t *testing.T) {
	rf, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	// handled 记录引擎里写死了编号的判定项。新增规则时必须同步加到这里和引擎里，
	// 这样规则表的改动不会悄悄绕过判定。
	handled := map[string]bool{
		"R01-a": true, "R01-b": true, "R01-c": true,
		"R02-a": true, "R02-b": true,
		"R03-a": true, "R03-b": true,
		"R04-a": true, "R04-b": true,
		"R05-a": true, "R05-b": true,
		"R06-a": true, "R06-b": true,
		"R07-a": true,
		"R08-a": true,
		"R09-a": true, "R09-b": true,
		"R10-a": true,
		"R11-a": true,
		"R12-a": true,
		"R13-a": true, "R13-b": true,
		"R14-a": true, "R14-b": true,
		"R15-a": true, "R15-b": true,
	}
	all := rf.AllCheckIDs()
	if len(all) != len(handled) {
		t.Fatalf("规则表判定项 %d 个，引擎覆盖 %d 个，数量不一致", len(all), len(handled))
	}
	for _, id := range all {
		if !handled[id] {
			t.Errorf("规则表有判定项 %s，引擎未实现", id)
		}
	}
}

// 每个判定项命中时都必须带一句用实际数值写成的依据，评审要看到"为什么这么判"。
func TestFindingsAlwaysExplain(t *testing.T) {
	rf, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	cases, err := loadCases()
	if err != nil {
		t.Fatal(err)
	}
	eng := New(rf)
	seen := map[string]bool{}
	for _, c := range cases {
		res := eng.Judge(InputFromCase(c))
		for _, f := range res.Findings {
			if f.Reason == "" {
				t.Errorf("%s 判定项 %s 命中但没有依据", c.ID, f.CheckID)
			}
			if f.Clause == "" || f.ClauseText == "" {
				t.Errorf("%s 判定项 %s 缺少条款编号或条款原文", c.ID, f.CheckID)
			}
			// 依据里应出现具体数字或名称，而不是只有编号。
			if len([]rune(f.Reason)) < 6 {
				t.Errorf("%s 判定项 %s 的依据过于笼统: %q", c.ID, f.CheckID, f.Reason)
			}
			seen[f.CheckID] = true
		}
		if res.Decision != rf.Aggregate(res.Hits) {
			t.Errorf("%s 结论与命中项聚合不一致", c.ID)
		}
		for _, id := range res.Hits {
			if !seen[id] {
				t.Errorf("%s 命中项 %s 未出现在 Findings 中", c.ID, id)
			}
		}
	}
}

// 跳过项必须写明原因，否则人工复核时不知道这张单据"哪些没查"。
func TestSkippedAlwaysHasReason(t *testing.T) {
	rf, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	cases, err := loadCases()
	if err != nil {
		t.Fatal(err)
	}
	eng := New(rf)
	for _, c := range cases {
		res := eng.Judge(InputFromCase(c))
		for id, why := range res.Skipped {
			if why == "" {
				t.Errorf("%s 判定项 %s 被跳过但没有原因", c.ID, id)
			}
			if _, _, ok := rf.FindCheck(id); !ok {
				t.Errorf("%s 跳过记录里出现规则表不存在的判定项 %s", c.ID, id)
			}
		}
	}
}
