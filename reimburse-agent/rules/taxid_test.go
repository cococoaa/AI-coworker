package rules

import "testing"

func TestValidTaxID(t *testing.T) {
	good := []string{"91350100M000100Y43", "91310115MA1FL8X3K0"}
	for _, id := range good {
		if !ValidTaxID(id) {
			t.Errorf("应当合法: %s", id)
		}
	}
	bad := []string{"", "91350100M000100Y44", "91310115MA1FL8X3K1", "91310115MA1FL8X3K", "9131011５MA1FL8X3K0", "91310115MA1FL8X3KI"}
	for _, id := range bad {
		if ValidTaxID(id) {
			t.Errorf("应当不合法: %q", id)
		}
	}
}

func TestLoadEmbedded(t *testing.T) {
	f, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidTaxID(f.Params.Company.TaxID) {
		t.Errorf("公司税号校验位不合法: %s", f.Params.Company.TaxID)
	}
	if len(f.AllCheckIDs()) != 26 {
		t.Errorf("判定项数量=%d，期望 26", len(f.AllCheckIDs()))
	}
	if got := f.Aggregate([]string{"R07-a", "R10-a"}); got != NeedMoreInfo {
		t.Errorf("聚合结果=%s", got)
	}
	if got := f.Aggregate([]string{"R02-b", "R07-a"}); got != Reject {
		t.Errorf("聚合结果=%s", got)
	}
	if got := f.Aggregate(nil); got != Approve {
		t.Errorf("聚合结果=%s", got)
	}
}
