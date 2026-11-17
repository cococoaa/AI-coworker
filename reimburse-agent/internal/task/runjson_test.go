package task

import (
	"encoding/json"
	"testing"
)

// 通过的批次在 JSON 里必须是 "hits": []，不能是 null。
//
// 理由：界面上"命中 0 项"是干净通过，"没有命中记录"是数据缺失。
// Go 的 encoding/json 对 nil slice 永远输出 null，光改 struct tag 无效，
// 必须在写入前填成空切片。这条测试就是锁死这个细节。
func TestRunHitsSerializesAsEmptyArray(t *testing.T) {
	s := newStore(t)
	tk, _ := s.Create("C01", "BX26110001", "E1001")
	// 干净通过：没有任何命中项。
	got, err := s.ApplyDecision(tk.ID, Run{Decision: "approve", ApprovedFen: 55300},
		"approve", nil, nil, "E3001")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Runs) != 1 {
		t.Fatalf("应记录 1 个批次，实际 %d", len(got.Runs))
	}

	b, err := json.Marshal(got.Runs[0])
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	hits, ok := raw["hits"]
	if !ok {
		t.Fatal("hits 字段必须存在，不能省略")
	}
	if string(hits) != "[]" {
		t.Errorf("通过批次的 hits 应为空数组，实际 %s", hits)
	}

	// 命中多条时正常输出。
	got, err = s.ApplyDecision(tk.ID, Run{Decision: "reject", Hits: []string{"R01-a", "R02-b"}},
		"reject", nil, nil, "E3001")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(got.Runs[1])
	raw = map[string]json.RawMessage{}
	_ = json.Unmarshal(b, &raw)
	if string(raw["hits"]) != `["R01-a","R02-b"]` {
		t.Errorf("命中项应原样输出，实际 %s", raw["hits"])
	}
}
