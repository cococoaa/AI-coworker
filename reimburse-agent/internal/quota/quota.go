// Package quota 是 AI 用量的周预算控制。
//
// 三条设计原则：
//
//  1. **计数必须落盘。** 内存计数一重启就归零，周限额等于没有。这里用一个小 JSON 文件
//     加原子写（临时文件 + rename），不需要数据库。
//
//  2. **超限只熔断，不降级。** 预算耗尽时停止自动判定、全部转人工，绝不用更便宜的模型
//     凑合判一个结论。积压是可见的，错批是不可见的——这条与工具超时、字段不可信
//     走的是同一条路。
//
//  3. **按调用方分桶。** 只记总量的话，报警了也不知道该找谁。令牌名就是调用方，
//     天然可以分桶，不需要额外的人员体系。
//
// 周口径用 ISO 周（周一为一周起点）。跨周自动开新桶，旧周数据留在文件里，
// 便于看趋势与复盘。
package quota

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Level 是预算使用程度。数值越大越严重。
type Level int

const (
	// OK 正常。
	OK Level = iota
	// Warn 达到预警线，继续但提醒。
	Warn
	// Critical 达到严重线，继续但强提醒。
	Critical
	// Blocked 预算耗尽，熔断：不再自动判定。
	Blocked
)

// MarshalJSON 把档位序列化成字符串（"ok"/"warn"/"critical"/"blocked"）。
// 默认会序列化成数字 0..3，前端按字符串比较就会全部失效——这是实测踩到的。
func (l Level) MarshalJSON() ([]byte, error) {
	return []byte(`"` + l.String() + `"`), nil
}

// UnmarshalJSON 接受字符串与数字两种形态：
// 字符串是现在写的，数字是早期版本写的，都要能读回来（向前兼容）。
func (l *Level) UnmarshalJSON(b []byte) error {
	t := strings.Trim(string(b), `"`)
	switch t {
	case "ok":
		*l = OK
	case "warn":
		*l = Warn
	case "critical":
		*l = Critical
	case "blocked":
		*l = Blocked
	case "0":
		*l = OK
	case "1":
		*l = Warn
	case "2":
		*l = Critical
	case "3":
		*l = Blocked
	default:
		*l = OK
	}
	return nil
}

func (l Level) String() string {
	switch l {
	case OK:
		return "ok"
	case Warn:
		return "warn"
	case Critical:
		return "critical"
	case Blocked:
		return "blocked"
	}
	return "unknown"
}

// Config 是配额配置。
type Config struct {
	// WeeklyBudgetUSD 是周预算（美元）。0 表示不限。
	WeeklyBudgetUSD float64
	// WarnRatio 预警线，例如 0.7。
	WarnRatio float64
	// CriticalRatio 严重线，例如 0.9。
	CriticalRatio float64
	// PerCaseAlertUSD 单张单据成本告警线。超过它说明这一单异常（重试风暴、
	// 超长文本），即使总量没超也要报出来。
	PerCaseAlertUSD float64
	// KeepWeeks 保留最近多少周的历史。
	KeepWeeks int
}

// DefaultConfig 返回保守的默认值。
//
// 依据：真实评测平均 $0.0101/单。按 200 单/工作日算是 $2/天，一周约 $10。
// 周预算设 $50 大约是正常消耗的 5 倍，留足业务高峰余量又能在失控时拦住。
func DefaultConfig() Config {
	return Config{
		WeeklyBudgetUSD: 50,
		WarnRatio:       0.7,
		CriticalRatio:   0.9,
		PerCaseAlertUSD: 0.05,
		KeepWeeks:       8,
	}
}

// Alert 是一条告警记录。
type Alert struct {
	At      time.Time `json:"at"`
	Level   Level     `json:"level"`
	Client  string    `json:"client,omitempty"`
	CaseID  string    `json:"case_id,omitempty"`
	Message string    `json:"message"`
}

// Status 是某一时刻的用量快照。
type Status struct {
	Week      string             `json:"week"`
	UsedUSD   float64            `json:"used_usd"`
	BudgetUSD float64            `json:"budget_usd"`
	Ratio     float64            `json:"ratio"`
	Level     Level              `json:"level"`
	ByClient  map[string]float64 `json:"by_client"`
	// RemainingUSD 剩余预算；不限时为负数，界面据此显示"不限"。
	RemainingUSD float64 `json:"remaining_usd"`
	// Blocked 为真表示已熔断，不应再启动自动审核。
	Blocked bool `json:"blocked"`
	// Alerts 是本周告警（最近在前）。
	Alerts []Alert `json:"alerts,omitempty"`
}

// Store 是配额存储。并发安全。
type Store struct {
	path string
	cfg  Config

	mu   sync.Mutex
	data fileData
	// now 便于测试注入固定时间。
	now func() time.Time
}

// fileData 是落盘结构。
type fileData struct {
	Version int              `json:"version"`
	Note    string           `json:"note,omitempty"`
	Weeks   map[string]*week `json:"weeks"`
}

type week struct {
	TotalUSD float64            `json:"total_usd"`
	ByClient map[string]float64 `json:"by_client"`
	Alerts   []Alert            `json:"alerts"`
}

// Open 打开或创建配额文件。文件不存在时按空配额起步。
func Open(path string, cfg Config) (*Store, error) {
	if cfg.WeeklyBudgetUSD < 0 {
		return nil, fmt.Errorf("周预算不能为负")
	}
	if cfg.WarnRatio <= 0 || cfg.WarnRatio >= 1 {
		cfg.WarnRatio = 0.7
	}
	if cfg.CriticalRatio <= cfg.WarnRatio || cfg.CriticalRatio > 1 {
		cfg.CriticalRatio = 0.9
	}
	if cfg.KeepWeeks <= 0 {
		cfg.KeepWeeks = 8
	}
	s := &Store{path: path, cfg: cfg, now: time.Now,
		data: fileData{Version: 1, Weeks: map[string]*week{}}}
	b, err := os.ReadFile(path)
	if err == nil {
		var d fileData
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("配额文件损坏 %s: %w", path, err)
		}
		if d.Weeks == nil {
			d.Weeks = map[string]*week{}
		}
		d.Version = 1
		s.data = d
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

// WeekKey 返回时间点所属的 ISO 周，例如 "2026-W47"。
func WeekKey(t time.Time) string {
	y, w := t.ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// current 返回本周的桶（不存在则创建）。调用方必须持有锁。
func (s *Store) current() *week {
	k := WeekKey(s.now())
	w, ok := s.data.Weeks[k]
	if !ok {
		w = &week{ByClient: map[string]float64{}}
		s.data.Weeks[k] = w
	}
	if w.ByClient == nil {
		w.ByClient = map[string]float64{}
	}
	return w
}

// level 由总量与预算算出档位。调用方必须持有锁。
func (s *Store) level(total float64) Level {
	if s.cfg.WeeklyBudgetUSD <= 0 {
		return OK
	}
	r := total / s.cfg.WeeklyBudgetUSD
	switch {
	case r >= 1:
		return Blocked
	case r >= s.cfg.CriticalRatio:
		return Critical
	case r >= s.cfg.WarnRatio:
		return Warn
	}
	return OK
}

// Check 是审核前的预检。返回当前状态；若 Blocked，调用方必须停止自动审核。
//
// 这里只读不写：预检不应该因为"有人点了一下"就改变计数。
func (s *Store) Check(client string) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked(client)
}

func (s *Store) statusLocked(client string) Status {
	w := s.current()
	lv := s.level(w.TotalUSD)
	st := Status{
		Week:      WeekKey(s.now()),
		UsedUSD:   round6(w.TotalUSD),
		BudgetUSD: s.cfg.WeeklyBudgetUSD,
		Level:     lv,
		ByClient:  map[string]float64{},
		Blocked:   lv == Blocked,
	}
	if s.cfg.WeeklyBudgetUSD > 0 {
		st.Ratio = w.TotalUSD / s.cfg.WeeklyBudgetUSD
		st.RemainingUSD = s.cfg.WeeklyBudgetUSD - w.TotalUSD
	} else {
		st.RemainingUSD = -1
	}
	for k, v := range w.ByClient {
		st.ByClient[k] = round6(v)
	}
	// 告警最近在前，最多 10 条，避免界面被刷屏。
	for i := len(w.Alerts) - 1; i >= 0 && len(st.Alerts) < 10; i-- {
		st.Alerts = append(st.Alerts, w.Alerts[i])
	}
	return st
}

// Record 记一次实际消耗。返回记录后的状态。
//
// 两件事会触发告警：
//   - 跨越预警/严重/熔断线（每跨一次记一条，不重复记）；
//   - 单张单据成本超过 PerCaseAlertUSD（抓重试风暴与超长文本）。
func (s *Store) Record(client string, costUSD float64, caseID string) Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	w := s.current()
	before := s.level(w.TotalUSD)
	w.TotalUSD += costUSD
	if client != "" {
		w.ByClient[client] += costUSD
	}
	after := s.level(w.TotalUSD)

	if after > before {
		w.Alerts = append(w.Alerts, Alert{
			At: s.now(), Level: after, Client: client, CaseID: caseID,
			Message: fmt.Sprintf("周用量达到 %.1f%%（$%.4f / $%.2f），档位 %s",
				ratioPct(w.TotalUSD, s.cfg.WeeklyBudgetUSD), w.TotalUSD, s.cfg.WeeklyBudgetUSD, after),
		})
	}
	if s.cfg.PerCaseAlertUSD > 0 && costUSD > s.cfg.PerCaseAlertUSD {
		w.Alerts = append(w.Alerts, Alert{
			At: s.now(), Level: Critical, Client: client, CaseID: caseID,
			Message: fmt.Sprintf("单张单据成本 $%.4f 超过告警线 $%.2f，疑似重试或超长文本",
				costUSD, s.cfg.PerCaseAlertUSD),
		})
	}

	s.trimLocked()
	_ = s.saveLocked()
	return s.statusLocked(client)
}

// AddUsage 直接给本周加上一笔用量，不产生告警。仅供演示预置用量。
func (s *Store) AddUsage(client string, usd float64) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.current()
	w.TotalUSD += usd
	if client != "" {
		w.ByClient[client] += usd
	}
	_ = s.saveLocked()
	return s.statusLocked(client)
}

// Reset 清空本周用量。仅供演示重置。
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Weeks[WeekKey(s.now())] = &week{ByClient: map[string]float64{}}
	return s.saveLocked()
}

// saveLocked 原子落盘：先写临时文件再 rename，避免写一半断电把文件弄坏。
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// trimLocked 只保留最近 KeepWeeks 周。
func (s *Store) trimLocked() {
	if len(s.data.Weeks) <= s.cfg.KeepWeeks {
		return
	}
	keys := make([]string, 0, len(s.data.Weeks))
	for k := range s.data.Weeks {
		keys = append(keys, k)
	}
	sort.Strings(keys) // "2026-W47" 字典序即时间序
	for i := 0; i < len(keys)-s.cfg.KeepWeeks; i++ {
		delete(s.data.Weeks, keys[i])
	}
}

// Config 返回生效的配置。
func (s *Store) Config() Config { return s.cfg }

// SetNow 注入时钟，测试用。
func (s *Store) SetNow(f func() time.Time) { s.now = f }

func ratioPct(used, budget float64) float64 {
	if budget <= 0 {
		return 0
	}
	return used / budget * 100
}

func round6(v float64) float64 {
	return float64(int64(v*1e6+0.5)) / 1e6
}
