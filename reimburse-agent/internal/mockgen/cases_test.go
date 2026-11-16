package mockgen

import (
	"encoding/json"
	"strings"
	"testing"

	"reimburse/rules"
)

const testSeed = 20261121

func load(t *testing.T) *rules.File {
	t.Helper()
	rf, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	return rf
}

func TestCasesSelfCheckAndCoverage(t *testing.T) {
	rf := load(t)
	cases, err := NewGen(testSeed, rf).Cases()
	if err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	for _, id := range Coverage(cases) {
		covered[id] = true
	}
	for _, id := range rf.AllCheckIDs() {
		if !covered[id] {
			t.Errorf("判定项 %s 无用例覆盖", id)
		}
	}
	ids := map[string]bool{}
	for _, c := range cases {
		if ids[c.ID] {
			t.Errorf("用例编号重复 %s", c.ID)
		}
		ids[c.ID] = true
	}
}

func TestDeterministic(t *testing.T) {
	rf := load(t)
	a, err := NewGen(testSeed, rf).Cases()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewGen(testSeed, rf).Cases()
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatal("同一种子两次生成的用例不一致")
	}
}

// 自检必须能抓到预期与标注不一致，否则"一次通过"没有意义。
func TestSelfCheckCatchesMismatch(t *testing.T) {
	rf := load(t)
	g := NewGen(testSeed, rf)
	b := &builder{g: g}
	c := g.newLodging(g.person("P"), g.cityOfTier("tier2"), 2, 36000, g.randStart())
	b.add(c, "故意写错预期", "R01-a", "test")
	if len(b.errs) == 0 || !strings.Contains(b.errs[0], "预期命中") {
		t.Fatalf("自检没有报出不一致: %v", b.errs)
	}
}
