package mcptools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"reimburse/internal/mcptools"
	"reimburse/internal/schema"
)

// 这些测试走真实的 MCP 协议：进程内连接 -> 工具发现 -> 调用 -> 结果解析。
// 不 mock 协议层，确保工具描述、入参 schema、结构化返回都真的能过 MCP。

// DemoStore 返回测试用的固定数据集。
func demoStore() *mcptools.Store {
	return mcptools.NewStore(schema.World{
		Invoices: []schema.RegInvoice{
			{InvoiceNo: "INV1", IssueDate: "2026-10-01", TotalFen: 55300, BuyerTaxID: "B1", SellerTaxID: "S1", Status: schema.InvNormal},
			{InvoiceNo: "INV2", IssueDate: "2026-10-02", TotalFen: 10000, BuyerTaxID: "B1", SellerTaxID: "S1", Status: schema.InvVoided},
		},
		Companies: []schema.Company{
			{TaxID: "S1", Name: "甲公司", City: "上海", Status: schema.CoActive},
			{TaxID: "S2", Name: "乙公司", City: "上海", Status: schema.CoCanceled},
		},
		History: []schema.HistRecord{
			{ClaimNo: "H1", InvoiceNo: "INV1", ApplicantID: "E1", SellerTaxID: "S1", IssueDate: "2026-10-01", AmountFen: 55300, Category: "transport", Status: schema.HistApproved},
			{ClaimNo: "H2", InvoiceNo: "INV9", ApplicantID: "E1", SellerTaxID: "S3", IssueDate: "2026-10-04", AmountFen: 30000, Category: "meal", Status: schema.HistApproved},
			{ClaimNo: "H3", InvoiceNo: "INV8", ApplicantID: "E2", SellerTaxID: "S3", IssueDate: "2026-10-05", AmountFen: 20000, Category: "meal", Status: schema.HistRejected},
		},
	})
}

// connect 建一对内存传输，返回已连上的客户端 mcptools.Remote。
func connect(t *testing.T, opt mcptools.Options) (*mcptools.Remote, *mcp.ClientSession) {
	t.Helper()
	if opt.Data == nil {
		opt.Data = demoStore()
	}
	srv := mcptools.NewServer(opt)
	cs, ss := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.MCP().Connect(ctx, ss, nil); err != nil {
		t.Fatal(err)
	}
	sess, err := mcptools.Client(ctx, cs, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return &mcptools.Remote{Session: sess, Timeout: 5 * time.Second}, sess
}

// 工具必须注册齐全，且入参 schema 里真的带上了字段——否则模型无从填写参数。
func TestToolsRegisteredWithSchema(t *testing.T) {
	_, sess := connect(t, mcptools.Options{})
	names, err := (&mcptools.Remote{Session: sess}).ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{mcptools.ToolVerifyInvoice: false, mcptools.ToolCheckCompany: false, mcptools.ToolCheckHistory: false}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for n, ok := range want {
		if !ok {
			t.Errorf("工具 %s 未注册", n)
		}
	}
	res, err := sess.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range res.Tools {
		b, _ := json.Marshal(tl.InputSchema)
		if !strings.Contains(string(b), "invoice_no") && !strings.Contains(string(b), "tax_id") {
			t.Errorf("工具 %s 的入参 schema 里没有业务字段: %s", tl.Name, b)
		}
		if tl.Description == "" {
			t.Errorf("工具 %s 缺少描述", tl.Name)
		}
	}
}

func TestVerifyFound(t *testing.T) {
	r, _ := connect(t, mcptools.Options{})
	tr := r.VerifyInvoice(context.Background(), "INV1")
	if tr.Status != mcptools.ResultOK {
		t.Fatalf("应成功: %+v", tr)
	}
	if len(tr.Invoices) != 1 || tr.Invoices[0].TotalFen != 55300 || tr.Invoices[0].Status != schema.InvNormal {
		t.Fatalf("返回数据错误: %+v", tr.Invoices)
	}
}

// 查无此票是确定事实，状态必须是 ok；上层据此判 R01-b 驳回。
func TestVerifyNotFoundIsOK(t *testing.T) {
	r, _ := connect(t, mcptools.Options{})
	tr := r.VerifyInvoice(context.Background(), "NOPE")
	if tr.Status != mcptools.ResultOK {
		t.Fatalf("查无此票应为 ok，得到 %s", tr.Status)
	}
	if len(tr.Invoices) != 0 {
		t.Fatalf("不应返回记录: %+v", tr.Invoices)
	}
}

func TestCompanyStates(t *testing.T) {
	r, _ := connect(t, mcptools.Options{})
	for tax, want := range map[string]string{"S1": schema.CoActive, "S2": schema.CoCanceled} {
		tr := r.CheckCompany(context.Background(), tax)
		if tr.Status != mcptools.ResultOK || len(tr.Companies) != 1 || tr.Companies[0].Status != want {
			t.Errorf("%s 查询错误: %+v", tax, tr)
		}
	}
	tr := r.CheckCompany(context.Background(), "S404")
	if tr.Status != mcptools.ResultOK || len(tr.Companies) != 0 {
		t.Errorf("查无企业应为 ok 且空列表: %+v", tr)
	}
}

// 查重按发票号 + 申请人取并集，且不能把别人的记录带回来。
func TestHistoryScoped(t *testing.T) {
	r, _ := connect(t, mcptools.Options{})
	tr := r.CheckHistory(context.Background(), mcptools.HistoryQuery{InvoiceNo: "INV1", ApplicantID: "E1"})
	if tr.Status != mcptools.ResultOK {
		t.Fatalf("应成功: %+v", tr)
	}
	if len(tr.History) != 2 {
		t.Fatalf("应返回 E1 的 2 条记录，得到 %d 条: %+v", len(tr.History), tr.History)
	}
	for _, h := range tr.History {
		if h.ApplicantID != "E1" {
			t.Errorf("返回了不属于该申请人的记录: %+v", h)
		}
	}
	if tr.History[0].IssueDate < tr.History[1].IssueDate {
		t.Errorf("应按开票日期倒序: %+v", tr.History)
	}
	// 只按销售方查时应能拿到他人记录，这是查重需要的。
	tr = r.CheckHistory(context.Background(), mcptools.HistoryQuery{SellerTaxID: "S3"})
	if len(tr.History) != 2 {
		t.Errorf("按销售方应返回 2 条: %+v", tr.History)
	}
}

// 金额必须能在"分"与"元字符串"之间无损往返。
func TestAmountRoundTrip(t *testing.T) {
	for _, c := range []struct {
		fen int64
		s   string
	}{{55300, "553.00"}, {1, "0.01"}, {100000, "1000.00"}, {9, "0.09"}} {
		got, err := mcptools.ParseYuan(mcptools.Yuan(c.fen))
		if err != nil || got != c.fen {
			t.Errorf("%d -> %q -> %d (%v)", c.fen, mcptools.Yuan(c.fen), got, err)
		}
		if mcptools.Yuan(c.fen) != c.s {
			t.Errorf("元字符串格式错误: %q 应为 %q", mcptools.Yuan(c.fen), c.s)
		}
	}
	if _, err := mcptools.ParseYuan("abc"); err == nil {
		t.Error("非法金额应报错")
	}
}

// 故障注入：三种故障都必须落到 failed 或 timeout，绝不能变成"查无此票"。
func TestFaultInjection(t *testing.T) {
	cases := []struct {
		mode string
		want mcptools.ResultStatus
	}{
		{"timeout", mcptools.ResultTimeout},
		{"error", mcptools.ResultFailed},
		{"garbled", mcptools.ResultFailed},
	}
	for _, c := range cases {
		r, _ := connect(t, mcptools.Options{Data: demoStore(), Fault: map[string]string{mcptools.ToolVerifyInvoice: c.mode},
			FaultDelay: 50 * time.Millisecond})
		tr := r.VerifyInvoice(context.Background(), "INV1")
		if tr.Status != c.want {
			t.Errorf("%s 故障应判为 %s，得到 %s", c.mode, c.want, tr.Status)
		}
		if len(tr.Invoices) != 0 {
			t.Errorf("%s 故障不应返回发票数据", c.mode)
		}
		if tr.Detail == "" {
			t.Errorf("%s 故障应带原因说明", c.mode)
		}
	}
}

// 按 key 注入：只让某一张发票超时，其他单据正常。
func TestFaultScopedByKey(t *testing.T) {
	r, _ := connect(t, mcptools.Options{Data: demoStore(), Fault: map[string]string{
		mcptools.ToolCheckCompany + ":S1": "timeout"}, FaultDelay: 30 * time.Millisecond})
	if tr := r.CheckCompany(context.Background(), "S1"); tr.Status != mcptools.ResultTimeout {
		t.Errorf("S1 应超时，得到 %s", tr.Status)
	}
	if tr := r.CheckCompany(context.Background(), "S2"); tr.Status != mcptools.ResultOK {
		t.Errorf("S2 不应受影响，得到 %s", tr.Status)
	}
}

// 客户端超时必须判为 timeout 而不是 failed，两者对规则的语义不同。
func TestClientTimeout(t *testing.T) {
	r, _ := connect(t, mcptools.Options{Data: demoStore(), Fault: map[string]string{mcptools.ToolVerifyInvoice: "timeout"},
		FaultDelay: 2 * time.Second})
	r.Timeout = 200 * time.Millisecond
	tr := r.VerifyInvoice(context.Background(), "INV1")
	if tr.Status != mcptools.ResultTimeout {
		t.Fatalf("客户端放弃等待应判为 timeout，得到 %s (%s)", tr.Status, tr.Detail)
	}
}

// 服务端返回乱码且没有结构化内容时，必须判 failed，不能解析成空列表。
func TestGarbledTextRejected(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "bad", Version: "0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: mcptools.ToolVerifyInvoice}, func(ctx context.Context, req *mcp.CallToolRequest, in mcptools.InInvoice) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "▚▞░▒██"}}}, nil, nil
	})
	cs, ss := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), ss, nil); err != nil {
		t.Fatal(err)
	}
	sess, err := mcptools.Client(context.Background(), cs, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	r := &mcptools.Remote{Session: sess, Timeout: 2 * time.Second}
	tr := r.VerifyInvoice(context.Background(), "INV1")
	if tr.Status != mcptools.ResultFailed {
		t.Fatalf("乱码应判 failed，得到 %s", tr.Status)
	}
}

// 参数缺失属于调用方错误：服务端按协议错误返回，客户端判 failed 并留下原因。
func TestMissingArgument(t *testing.T) {
	r, _ := connect(t, mcptools.Options{})
	tr := r.VerifyInvoice(context.Background(), "")
	if tr.Status != mcptools.ResultFailed {
		t.Fatalf("空发票号应判 failed，得到 %s", tr.Status)
	}
	if !strings.Contains(tr.Detail, "invoice_no") {
		t.Errorf("应说明缺哪个参数: %s", tr.Detail)
	}
}

// OnCall 回调必须拿到原始返回，界面要靠它展示"工具到底返回了什么"。
func TestOnCallRaw(t *testing.T) {
	r, _ := connect(t, mcptools.Options{})
	var gotRaw, gotTool string
	r.OnCall = func(tool string, args any, res mcptools.Result, raw string, d time.Duration) {
		gotTool, gotRaw = tool, raw
	}
	r.VerifyInvoice(context.Background(), "INV1")
	if gotTool != mcptools.ToolVerifyInvoice || !strings.Contains(gotRaw, "INV1") {
		t.Fatalf("回调没有拿到原始返回: tool=%q raw=%q", gotTool, gotRaw)
	}
}
