package judge

import "testing"

// 这三条测试锁的是"原件没有"与"看不清"必须分开建模。
//
// 背景：C24（个人抬头且无购买方税号）在真实评测里出现过一次偏差——
// 模型正确读出"这张票没有税号"，但对空值只给了 0.5 置信度，
// 系统把它当成"识别失败"，于是 R02-b 不作判定、改由 R15-b 转人工。
// 结论仍对，依据却错了：该说"没税号"，说成了"读不清税号"。

func TestFieldStatusUsable(t *testing.T) {
	// 关键：Absent 可用作判定依据，Unreadable/Ambiguous 不可用。
	if !Present.Usable() {
		t.Error("Present 应可用")
	}
	if !Absent.Usable() {
		t.Error("Absent 应可用——原件没有这一栏是可判定的业务事实")
	}
	if Unreadable.Usable() || Ambiguous.Usable() {
		t.Error("Unreadable 与 Ambiguous 不应用作判定依据")
	}
	if !Unreadable.NeedsEvidence() || !Ambiguous.NeedsEvidence() {
		t.Error("Unreadable 与 Ambiguous 需要补件或人工")
	}
	if Absent.NeedsEvidence() {
		t.Error("Absent 是业务事实，不需要补件")
	}
}

// 显式 presence 优先：同样是空值，absent 与 unreadable 判定结果不同。
func TestClassifyByPresence(t *testing.T) {
	cases := []struct {
		name   string
		f      Field
		want   FieldStatus
		decide bool // 是否应由 presence 直接判定
	}{
		{"确认原件没有", Field{Value: "", Conf: 1, Presence: PresenceAbsent}, Absent, true},
		{"看不清", Field{Value: "", Conf: 0.3, Presence: PresenceUnreadable}, Unreadable, true},
		{"有值且可信", Field{Value: "91310115MA1FL8X3K0", Conf: 1, Presence: PresencePresent}, Present, true},
		{"有值但没把握", Field{Value: "91310115MA1FL8X3K", Conf: 0.5, Presence: PresencePresent}, Unreadable, true},
		// 自称 absent 却有值：自相矛盾，交给调用方按值推断（decided=false）。
		{"声称没有却有值", Field{Value: "X", Conf: 1, Presence: PresenceAbsent}, Unknown, false},
		{"声称有值却是空", Field{Value: "", Conf: 1, Presence: PresencePresent}, Unreadable, true},
	}
	for _, c := range cases {
		got, decided := ClassifyFieldByPresence(FBuyerTaxID, c.f, false, 0.8)
		if decided != c.decide {
			t.Errorf("%s: decided 期望 %v，得到 %v", c.name, c.decide, decided)
			continue
		}
		if c.decide && got != c.want {
			t.Errorf("%s: 期望 %v，得到 %v", c.name, c.want, got)
		}
	}
	// 两模型不一致优先于 presence。
	got, decided := ClassifyFieldByPresence(FBuyerTaxID, Field{Value: "A", Conf: 1, Presence: PresencePresent}, true, 0.8)
	if !decided || got != Ambiguous {
		t.Errorf("两模型不一致应判为 Ambiguous，得到 %v", got)
	}
}

// 没有 presence 时按置信度推断：两侧都高置信度地给出空 → Absent。
func TestClassifyFallback(t *testing.T) {
	high := Field{Value: "", Conf: 1}
	low := Field{Value: "", Conf: 0.5}

	if st := ClassifyField(FBuyerTaxID, high, high, true, true, false, 0.8); st != Absent {
		t.Errorf("两侧都高置信度给出空 → 应判 Absent，得到 %v", st)
	}
	if st := ClassifyField(FBuyerTaxID, low, low, true, true, false, 0.8); st != Unreadable {
		t.Errorf("两侧都低置信度给出空 → 应判 Unreadable，得到 %v", st)
	}
	if st := ClassifyField(FBuyerTaxID, Field{Value: "X", Conf: 1}, Field{Value: "X", Conf: 1}, true, true, false, 0.8); st != Present {
		t.Errorf("有值且可信 → Present，得到 %v", st)
	}
}

// 端到端：原件确实没税号，应命中 R02-b 并驳回，而不是只靠 R15-b 转人工。
func TestAbsentTaxIDHitsR02b(t *testing.T) {
	f, err := loadFixturePolicy(t)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := loadFixtures(t)
	if err != nil {
		t.Fatal(err)
	}
	eng := New(f)

	// 取一张本来通过的干净单据，把税号改成"确认原件没有"。
	var base *fixtureCase
	for _, c := range cases {
		if c.Truth.Decision == "approve" {
			base = c
			break
		}
	}
	if base == nil {
		t.Skip("没有干净用例")
	}
	in := cloneInput(base.Input)
	in.Fields[FBuyerTaxID] = Field{Value: "", Conf: 1, Presence: PresenceAbsent}
	in.Untrusted = map[FieldName]bool{}

	res := eng.Judge(in)
	if !hasHit(res.Hits, "R02-b") {
		t.Errorf("原件确认无税号应命中 R02-b，实际命中 %v", res.Hits)
	}
	if hasHit(res.Hits, "R15-b") {
		t.Errorf("Absent 不应触发 R15-b（它不是识别失败），实际命中 %v", res.Hits)
	}
	if res.Decision != "reject" {
		t.Errorf("缺购买方税号应驳回，得到 %s", res.Decision)
	}
	if res.FieldStatus.Of(FBuyerTaxID) != Absent {
		t.Errorf("字段状态应为 absent，得到 %v", res.FieldStatus.Of(FBuyerTaxID))
	}
}

// 对照：看不清税号时应转人工（R15-b），而不是判"没有税号"驳回。
func TestUnreadableTaxIDGoesManual(t *testing.T) {
	f, err := loadFixturePolicy(t)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := loadFixtures(t)
	if err != nil {
		t.Fatal(err)
	}
	eng := New(f)
	var base *fixtureCase
	for _, c := range cases {
		if c.Truth.Decision == "approve" {
			base = c
			break
		}
	}
	in := cloneInput(base.Input)
	in.Fields[FBuyerTaxID] = Field{Value: "", Conf: 0.3, Presence: PresenceUnreadable}
	in.Untrusted = map[FieldName]bool{}

	res := eng.Judge(in)
	if hasHit(res.Hits, "R02-b") {
		t.Errorf("看不清税号不应判 R02-b，实际命中 %v", res.Hits)
	}
	if !hasHit(res.Hits, "R15-b") {
		t.Errorf("看不清应命中 R15-b，实际命中 %v", res.Hits)
	}
	if res.Decision == "reject" {
		t.Errorf("看不清不应直接驳回（那是拿猜测驳回），得到 %s", res.Decision)
	}
}

// 四个字段的缺失分类要能在结果里看到，供补件清单使用。
func TestStatusesSummary(t *testing.T) {
	st := Statuses{
		FBuyerTaxID:  Absent,
		FSellerTaxID: Unreadable,
		FInvoiceNo:   Present,
		FItemName:    Ambiguous,
	}
	if got := len(st.MissingFacts()); got != 1 {
		t.Errorf("应有 1 个'原件没有'，得到 %d", got)
	}
	if got := len(st.NeedsEvidence()); got != 2 {
		t.Errorf("应有 2 个需要补正，得到 %d", got)
	}
	if s := st.Summary(); s == "" {
		t.Error("摘要不应为空")
	}
}
