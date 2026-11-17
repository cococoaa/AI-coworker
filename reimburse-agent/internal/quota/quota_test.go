package quota

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T, cfg Config) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "quota.json"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// 固定时钟到 2026-11-17（周二），ISO 周应是 2026-W47。
	s.SetNow(func() time.Time { return time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC) })
	return s
}

func TestDefaultNotBlocked(t *testing.T) {
	s := newStore(t, DefaultConfig())
	st := s.Check("reviewer")
	if st.Level != OK || st.Blocked {
		t.Fatalf("空配额应为 OK，得到 %v", st.Level)
	}
	if st.Week != "2026-W47" {
		t.Errorf("周口径错误: %s", st.Week)
	}
}

// 三档阈值：70% 预警、90% 严重、100% 熔断。
func TestLevels(t *testing.T) {
	s := newStore(t, DefaultConfig()) // 预算 $50
	cases := []struct {
		used float64
		want Level
	}{
		{10, OK},       // 20%
		{35, Warn},     // 70%
		{45, Critical}, // 90%
		{50, Blocked},  // 100%
		{80, Blocked},  // 超支
	}
	for _, c := range cases {
		s.Reset()
		s.AddUsage("reviewer", c.used)
		st := s.Check("reviewer")
		if st.Level != c.want {
			t.Errorf("用量 $%.0f 应为 %v，得到 %v", c.used, c.want, st.Level)
		}
		if st.Blocked != (c.want == Blocked) {
			t.Errorf("用量 $%.0f 的 Blocked 标志不对", c.used)
		}
	}
}

// 预算耗尽必须熔断：这是防止"用便宜模型凑合判"的前提。
func TestBlockedStopsAutomation(t *testing.T) {
	s := newStore(t, DefaultConfig())
	s.AddUsage("reviewer", 30) // 60%
	if s.Check("reviewer").Blocked {
		t.Fatal("60% 不应熔断")
	}
	s.AddUsage("reviewer", 20) // 100%
	st := s.Check("reviewer")
	if !st.Blocked {
		t.Fatal("100% 应熔断")
	}
	if st.Level != Blocked {
		t.Errorf("档位应为 blocked，得到 %v", st.Level)
	}
	if st.RemainingUSD > 0 {
		t.Errorf("剩余预算应为 0 或负，得到 %.4f", st.RemainingUSD)
	}
}

// 跨越阈值要产生告警，且同一档位不重复记。
func TestAlertOnThresholdCross(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PerCaseAlertUSD = 0 // 本测试只验阈值告警，关掉单张告警线避免混淆
	s := newStore(t, cfg)
	s.Record("reviewer", 34, "C01") // 68%，未到 70%
	if n := countAlerts(s.Check("reviewer")); n != 0 {
		t.Fatalf("未到预警线不应告警，得到 %d 条", n)
	}
	s.Record("reviewer", 2, "C02") // 72%，跨预警线
	if n := countAlerts(s.Check("reviewer")); n != 1 {
		t.Fatalf("跨预警线应告警 1 条，得到 %d", n)
	}
	s.Record("reviewer", 1, "C03") // 74%，仍在预警档，不重复告警
	if n := countAlerts(s.Check("reviewer")); n != 1 {
		t.Fatalf("同档位不应重复告警，得到 %d", n)
	}
	s.Record("reviewer", 10, "C04") // 94%，跨严重线
	if n := countAlerts(s.Check("reviewer")); n != 2 {
		t.Fatalf("跨严重线应再告警 1 条，得到 %d", n)
	}
}

// 单张成本超标要单独告警，抓重试风暴。
func TestPerCaseAlert(t *testing.T) {
	s := newStore(t, DefaultConfig()) // 单张告警线 $0.05
	s.Record("reviewer", 0.01, "C01")
	if n := countAlerts(s.Check("reviewer")); n != 0 {
		t.Errorf("正常成本不应告警，得到 %d", n)
	}
	s.Record("reviewer", 0.42, "C02") // 42 倍
	st := s.Check("reviewer")
	if countAlerts(st) != 1 {
		t.Fatalf("单张超标应告警，得到 %d", countAlerts(st))
	}
	if st.Alerts[0].CaseID != "C02" {
		t.Errorf("告警应指向 C02，得到 %s", st.Alerts[0].CaseID)
	}
}

// 按调用方分桶：报警了要知道找谁。
func TestByClient(t *testing.T) {
	s := newStore(t, DefaultConfig())
	s.Record("reviewer", 1, "C01")
	s.Record("batch-script", 2, "C02")
	s.Record("reviewer", 0.5, "C03")
	st := s.Check("reviewer")
	if st.ByClient["reviewer"] != 1.5 || st.ByClient["batch-script"] != 2 {
		t.Errorf("分桶错误: %v", st.ByClient)
	}
	if st.UsedUSD != 3.5 {
		t.Errorf("总量错误: %.4f", st.UsedUSD)
	}
}

// 计数必须落盘：重启后还在。
func TestPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "quota.json")
	s1, err := Open(p, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	s1.SetNow(func() time.Time { return time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC) })
	s1.Record("reviewer", 12.5, "C01")

	// 重新打开，模拟进程重启。
	s2, err := Open(p, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	s2.SetNow(func() time.Time { return time.Date(2026, 11, 17, 11, 0, 0, 0, time.UTC) })
	st := s2.Check("reviewer")
	if st.UsedUSD != 12.5 {
		t.Fatalf("重启后用量应保留，得到 %.4f", st.UsedUSD)
	}
	if st.ByClient["reviewer"] != 12.5 {
		t.Errorf("重启后分桶应保留: %v", st.ByClient)
	}
}

// 跨周自动开新桶，旧周数据不丢。
func TestWeekRollover(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "quota.json")
	s, _ := Open(p, DefaultConfig())
	s.SetNow(func() time.Time { return time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC) })
	s.Record("reviewer", 40, "C01") // W47 用掉 40

	if s.Check("reviewer").Blocked {
		t.Fatalf("W47 40/50 是 80%%，不应熔断")
	}
	// 跳到下一周
	s.SetNow(func() time.Time { return time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC) })
	st := s.Check("reviewer")
	if st.Week != "2026-W48" {
		t.Errorf("应进入 W48，得到 %s", st.Week)
	}
	if st.UsedUSD != 0 {
		t.Errorf("新周用量应清零，得到 %.4f", st.UsedUSD)
	}
	if st.Blocked {
		t.Error("新周不应熔断")
	}

	// 旧周数据仍在文件里
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(string(b), "2026-W47") {
		t.Error("旧周数据不应被删除")
	}
}

// 不限预算时永远不熔断。
func TestUnlimitedBudget(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WeeklyBudgetUSD = 0
	s := newStore(t, cfg)
	s.AddUsage("reviewer", 1000)
	st := s.Check("reviewer")
	if st.Blocked {
		t.Fatal("不限预算不应熔断")
	}
	if st.RemainingUSD != -1 {
		t.Errorf("不限预算时 Remaining 应为 -1（界面显示'不限'），得到 %.2f", st.RemainingUSD)
	}
}

// 告警列表最多返回 10 条，避免界面被刷屏。
func TestAlertCap(t *testing.T) {
	s := newStore(t, DefaultConfig())
	for i := 0; i < 30; i++ {
		s.Record("reviewer", 0.2, "C01") // 每笔都超单张告警线
	}
	if n := len(s.Check("reviewer").Alerts); n > 10 {
		t.Errorf("告警最多 10 条，得到 %d", n)
	}
}

func countAlerts(st Status) int { return len(st.Alerts) }

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
