package identity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := New([]Person{
		{EmployeeID: "E1001", Name: "王会计", Dept: "财务", CanReview: true, CanAudit: true},
		{EmployeeID: "E1002", Name: "李工人", Dept: "研发", CanReview: false, CanAudit: true},
		{EmployeeID: "E1003", Name: "张三（已离职）", Disabled: true, CanReview: true, CanAudit: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// 最常见的两种动作：触发审核、写复核结论。
func TestAuthenticate(t *testing.T) {
	r := testRegistry(t)

	if _, err := r.Authenticate("E1001", "review"); err != nil {
		t.Errorf("王会计应能复核: %v", err)
	}
	if _, err := r.Authenticate("E1001", "audit"); err != nil {
		t.Errorf("王会计应能触发审核: %v", err)
	}
	// 李工人能跑审核，但不能签批。
	if _, err := r.Authenticate("E1002", "audit"); err != nil {
		t.Errorf("李工人应能触发审核: %v", err)
	}
	if _, err := r.Authenticate("E1002", "review"); err == nil {
		t.Error("李工人不应能复核")
	}
}

// 不在名单里的一律拒绝：默认拒绝，不是默认放行。
func TestUnknownRejected(t *testing.T) {
	r := testRegistry(t)
	_, err := r.Authenticate("E9999", "review")
	if !errors.Is(err, ErrUnknown) {
		t.Fatalf("未知工号应返回 ErrUnknown，得到 %v", err)
	}
	// 空工号也必须拒绝。
	if _, err := r.Authenticate("", "review"); err == nil {
		t.Error("空工号应拒绝")
	}
}

// 停用的人保留记录但拒绝操作。
func TestDisabledRejected(t *testing.T) {
	r := testRegistry(t)
	if _, err := r.Authenticate("E1003", "review"); err == nil {
		t.Fatal("停用工号应拒绝")
	}
	// 但仍能查到（保留历史记录用）。
	if p, ok := r.Lookup("E1003"); !ok || !p.Disabled {
		t.Error("停用的人仍应能查到")
	}
}

// 名单里工号不能重复，也不能为空。
func TestInvalidRegistry(t *testing.T) {
	if _, err := New([]Person{{EmployeeID: "E1"}, {EmployeeID: "E1"}}); err == nil {
		t.Error("重复工号应报错")
	}
	if _, err := New([]Person{{Name: "没有工号"}}); err == nil {
		t.Error("空工号应报错")
	}
}

// 文件不存在时是空名单：任何复核都被拒，而不是放行。
func TestMissingFileMeansDenyAll(t *testing.T) {
	r, err := LoadFile(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Size() != 0 {
		t.Fatalf("应得到空名单，得到 %d 人", r.Size())
	}
	if _, err := r.Authenticate("E1001", "review"); err == nil {
		t.Error("空名单时应拒绝所有操作")
	}
}

// 正常读文件。
func TestLoadFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "people.json")
	content := `{"people":[
		{"employee_id":"E2001","name":"周主管","dept":"财务","can_review":true,"can_audit":true},
		{"employee_id":"E2002","name":"吴助理","can_audit":true}
	]}`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Size() != 2 {
		t.Fatalf("应读到 2 人，得到 %d", r.Size())
	}
	if p, err := r.Authenticate("E2001", "review"); err != nil || p.Name != "周主管" {
		t.Errorf("周主管应能复核: %v", err)
	}
	if _, err := r.Authenticate("E2002", "review"); err == nil {
		t.Error("吴助理不应能复核")
	}
}

// All 按工号排序，便于界面稳定显示。
func TestAllSorted(t *testing.T) {
	r := testRegistry(t)
	all := r.All()
	for i := 1; i < len(all); i++ {
		if all[i-1].EmployeeID > all[i].EmployeeID {
			t.Fatalf("未按工号排序: %v", all)
		}
	}
}
