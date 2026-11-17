package server

import (
	"sync"
	"testing"

	"reimburse/internal/task"
)

// taskRun 造一个最小批次。
func taskRun(decision string) task.Run { return task.Run{Decision: decision} }

// missingItem 是 task.MissingItem 的短名。
type missingItem = task.MissingItem

// 连续点两次"续审"，只应启动一轮有效审核。
//
// 两次请求几乎同时到达，都读到"待审核"就都会跑一遍。
// 虽然后跑的会覆盖先跑的结论，但批次号、事件流、成本都会重复记一遍——
// 看起来像系统抽风，而且成本翻倍。这里用状态抢占保证只有一次能进。
func TestConcurrentReauditStartsOnce(t *testing.T) {
	s := testServer(t)
	c := mustCase(t, "C44")
	tk, err := s.Tasks.Create(c.ID, c.Claim.ClaimNo, c.Claim.Applicant.EmployeeID)
	if err != nil {
		t.Fatal(err)
	}
	// 造一个"待补材料且已补齐"的任务：这是续审的前置条件。
	if _, err := s.Tasks.ApplyDecision(tk.ID, taskRun("need_more_info"),
		"need_more_info", []missingItem{{
			Key: "support", Label: "行程单", Kind: "missing_attachment",
		}}, nil, "system"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Tasks.ResubmitIdem(tk.ID, "support", "行程单.txt", "E1001", "",
		"铁路电子客票 2026-10-15 上海虹桥 北京南 G102 553.00"); err != nil {
		t.Fatal(err)
	}

	// 同时抢两次。
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = s.Tasks.BeginAudit(tk.ID)
		}(i)
	}
	wg.Wait()

	ok := 0
	for _, e := range results {
		if e == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("并发抢占应只有一次成功，实际 %d 次", ok)
	}

	got, _ := s.Tasks.Get(tk.ID)
	if got.State != "auditing" {
		t.Errorf("抢占后应为审核中，实际 %s", got.State.Label())
	}

	// 释放后可以再抢（失败重试路径）。
	s.releaseTask(tk.ID)
	got, _ = s.Tasks.Get(tk.ID)
	if got.State != "pending" {
		t.Errorf("释放后应回到待审核，实际 %s", got.State.Label())
	}
	if _, err := s.Tasks.BeginAudit(tk.ID); err != nil {
		t.Errorf("释放后应能再次抢占: %v", err)
	}
}

// 已完成的任务不能再被抢占，否则会重开一轮没有意义的审核。
func TestBeginAuditRejectsFinished(t *testing.T) {
	s := testServer(t)
	tk, _ := s.Tasks.Create("C01", "BX26110001", "E1001")
	s.Tasks.ApplyDecision(tk.ID, taskRun("approve"), "approve", nil, nil, "system")

	if _, err := s.Tasks.BeginAudit(tk.ID); err == nil {
		t.Fatal("已通过的任务不应能再次抢占审核")
	}
}
