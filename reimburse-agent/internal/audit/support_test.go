package audit

import "testing"

// ParseSupport 的单元测试。解析错了会让 R09 席别、R12 本人等判定整体跑偏，必须锁死。

func TestParseSupportItineraryTrain(t *testing.T) {
	text := "铁路电子客票报销凭证\n乘车人:刘子豪\n乘车日期：2026-10-01\n车次：  G7305  上海站 → 宁波站\n席别：二等座\n票价：¥553.00\n合计:¥553.00\n"
	d := ParseSupport(text)
	if !d.Present || d.Kind != "itinerary" || d.Mode != "train" {
		t.Fatalf("类型识别错误: %+v", d)
	}
	if d.Person != "刘子豪" {
		t.Errorf("乘车人: %q", d.Person)
	}
	if d.Date != "2026-10-01" {
		t.Errorf("乘车日期: %q", d.Date)
	}
	if d.Seat != "二等座" {
		t.Errorf("席别: %q", d.Seat)
	}
	if !d.HasTotal || d.TotalFen != 55300 {
		t.Errorf("合计: %v %d", d.HasTotal, d.TotalFen)
	}
	if len(d.Lines) == 0 || d.Lines[0] != "票价" {
		t.Errorf("明细行: %v", d.Lines)
	}
}

func TestParseSupportItineraryFlight(t *testing.T) {
	text := "航空运输电子客票行程单\n旅客姓名：孙立群\n航班号：YF2345\n乘机日期：2026-09-30\n航程：上海 - 北京\n舱位：公务舱\n票价：¥2500.00\n燃油附加费：¥40.00\n民航发展基金：¥50.00\n合计：¥2590.00\n"
	d := ParseSupport(text)
	if d.Kind != "itinerary" || d.Mode != "flight" {
		t.Fatalf("类型识别错误: %+v", d)
	}
	if d.Person != "孙立群" || d.Seat != "公务舱" || d.Date != "2026-09-30" {
		t.Errorf("字段错误: %+v", d)
	}
	if d.TotalFen != 259000 {
		t.Errorf("合计: %d", d.TotalFen)
	}
}

func TestParseSupportFolio(t *testing.T) {
	text := "住宿结账单（水单）\n酒店：  望岳酒店(成都中心店)  \n入住人:王子轩\n入住日期:2026-09-30\n离店日期：2026-10-02\n共 2 晚\n逐日房费:\n2026-09-30  房费  ¥380.00\n2026-10-01  房费  ¥380.00\n合计：¥760.00\n"
	d := ParseSupport(text)
	if d.Kind != "folio" {
		t.Fatalf("类型识别错误: %+v", d)
	}
	if d.Person != "王子轩" || d.CheckIn != "2026-09-30" || d.CheckOut != "2026-10-02" || d.Nights != 2 {
		t.Errorf("字段错误: %+v", d)
	}
	if d.TotalFen != 76000 {
		t.Errorf("合计: %d", d.TotalFen)
	}
	// 逐日房费的品名应解析为「房费」，不能把日期也带进品名，否则禁止项目检查会误判。
	for _, l := range d.Lines {
		if l != "房费" {
			t.Errorf("明细行品名解析错误: %q", l)
		}
	}
}

func TestParseSupportConsumption(t *testing.T) {
	text := "消费清单\n商户：西安食尚汇\n消费日期：  2026-10-04\n品名  数量  金额\n宫保鸡丁  x1  ¥140.00\n53度茅台白酒  x1  ¥28.00\n合计：¥420.00\n"
	d := ParseSupport(text)
	if d.Kind != "consumption" || d.Date != "2026-10-04" || d.TotalFen != 42000 {
		t.Fatalf("字段错误: %+v", d)
	}
	if len(d.Lines) != 2 || d.Lines[1] != "53度茅台白酒" {
		t.Errorf("明细行: %v", d.Lines)
	}
}

func TestParseSupportEmpty(t *testing.T) {
	if d := ParseSupport("  \n "); d.Present {
		t.Fatalf("空白文本不应视为存在佐证凭证: %+v", d)
	}
}
