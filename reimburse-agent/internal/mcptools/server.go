package mcptools

import (
	"context"
	"encoding/json"
	"math/rand"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 服务端。三个工具注册在同一个 MCP Server 上，通过 stdio 或 Streamable HTTP 对外提供。
//
// 故障注入是服务端的显式能力（AUDIT_FAULT 环境变量或 FAULT 文件），
// 用于现场演示"工具超时/报错时系统怎么处理"，不是为了掩盖真实故障。

// Options 是服务端配置。
type Options struct {
	// Data 是数据来源。
	Data DataSource
	// Fault 是预置故障：工具名 -> 故障方式（timeout / error / garbled）。
	// 键可以用工具名（对所有调用生效），也可以用 "工具名:键值"（只对某个发票号或税号生效）。
	Fault map[string]string
	// FaultDelay 是 timeout 故障实际等待的时长，默认 3 秒。
	FaultDelay time.Duration
	// Now 便于测试注入固定时间。
	Now func() time.Time
	// Rnd 用于乱码故障，固定种子便于复现。
	Rnd *rand.Rand
}

// Server 是一个 MCP 服务实例。
type Server struct {
	mcp *mcp.Server
	opt Options
}

// NewServer 创建并注册三个工具。
func NewServer(opt Options) *Server {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.FaultDelay == 0 {
		opt.FaultDelay = 3 * time.Second
	}
	if opt.Rnd == nil {
		opt.Rnd = rand.New(rand.NewSource(20261121))
	}
	s := &Server{opt: opt}
	s.mcp = mcp.NewServer(&mcp.Implementation{
		Name:    "reimburse-verify",
		Version: "1.0.0",
	}, &mcp.ServerOptions{
		Instructions: "报销审核用的只读核验工具。三个工具都不会修改任何数据；" +
			"查无记录与调用失败是两件不同的事，调用方必须区分。",
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: ToolVerifyInvoice,
		Description: "在税务平台查验一张增值税电子发票是否真实存在、是否作废或红冲、开票日期与价税合计是否与登记的完全一致。" +
			"返回 found=false 表示查无此票（属确定事实）；status=failed 或 timeout 表示本次未能核验，不得推断发票真伪。",
	}, s.verify)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: ToolCheckCompany,
		Description: "查询销售方的企业登记状态（存续、注销、吊销、经营异常）。" +
			"返回 found=false 表示查无该税号；status=failed 或 timeout 表示本次未能核验。",
	}, s.company)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: ToolCheckHistory,
		Description: "按发票号码、申请人员工编号、销售方税号查询历史报销记录，用于识别同一张发票重复报销、换票重报和拆分报销。" +
			"三个条件至少提供一个；返回的记录按开票日期倒序。",
	}, s.history)

	return s
}

// MCP 返回底层 Server，供测试或 HTTP 挂载使用。
func (s *Server) MCP() *mcp.Server { return s.mcp }

// faultFor 判断本次调用是否需要注入故障。
// "tool:key" 优先于 "tool"，保证可以只让某一张发票超时。
func (s *Server) faultFor(tool, key string) string {
	if m := s.opt.Fault[tool+":"+key]; m != "" {
		return m
	}
	return s.opt.Fault[tool]
}

// garbled 生成一段无法使用的返回：字段名正确、值为随机乱码。
// 模拟的是"接口通了但数据坏了"，这种结果不能当作查无记录。
func (s *Server) garbled(n int) string {
	const chars = "▚▞░▒█%@#&*"
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteRune([]rune(chars)[s.opt.Rnd.Intn(len([]rune(chars)))])
	}
	return b.String()
}

func (s *Server) verify(ctx context.Context, req *mcp.CallToolRequest, in InInvoice) (*mcp.CallToolResult, *OutInvoice, error) {
	switch s.faultFor(ToolVerifyInvoice, in.InvoiceNo) {
	case "timeout":
		select {
		case <-time.After(s.opt.FaultDelay):
		case <-ctx.Done():
			// 客户端放弃等待，如实报告为超时。
		}
		o := OutInvoice{Status: StatusTimeout, Note: "税务平台响应超时"}
		return errResult("verify_invoice 超时，状态未知", o), &o, nil
	case "error":
		o := OutInvoice{Status: StatusFailed, Note: "税务平台 503"}
		return errResult("verify_invoice 返回错误：税务平台 503", o), &o, nil
	case "garbled":
		o := OutInvoice{Status: StatusFailed, Note: "返回体无法解析: " + s.garbled(24)}
		return errResult("verify_invoice 返回内容异常", o), &o, nil
	}
	out, err := Verify(s.opt.Data, in, s.opt.Now)
	if err != nil {
		return errResult(err.Error(), out), &out, nil
	}
	return okResult(out), &out, nil
}

func (s *Server) company(ctx context.Context, req *mcp.CallToolRequest, in InCompany) (*mcp.CallToolResult, *OutCompany, error) {
	key := strings.ToUpper(strings.TrimSpace(in.TaxID))
	switch s.faultFor(ToolCheckCompany, key) {
	case "timeout":
		select {
		case <-time.After(s.opt.FaultDelay):
		case <-ctx.Done():
		}
		o := OutCompany{Status: StatusTimeout, Note: "企业登记系统响应超时"}
		return errResult("check_company 超时，状态未知", o), &o, nil
	case "error":
		o := OutCompany{Status: StatusFailed, Note: "企业登记系统 500"}
		return errResult("check_company 返回错误：企业登记系统 500", o), &o, nil
	case "garbled":
		o := OutCompany{Status: StatusFailed, Note: "返回体无法解析: " + s.garbled(24)}
		return errResult("check_company 返回内容异常", o), &o, nil
	}
	out, err := CheckCompany(s.opt.Data, in, s.opt.Now)
	if err != nil {
		return errResult(err.Error(), out), &out, nil
	}
	return okResult(out), &out, nil
}

func (s *Server) history(ctx context.Context, req *mcp.CallToolRequest, in InHistory) (*mcp.CallToolResult, *OutHistory, error) {
	key := in.InvoiceNo
	if key == "" {
		key = in.ApplicantID
	}
	switch s.faultFor(ToolCheckHistory, key) {
	case "timeout":
		select {
		case <-time.After(s.opt.FaultDelay):
		case <-ctx.Done():
		}
		o := OutHistory{Status: StatusTimeout, Note: "历史库响应超时"}
		return errResult("check_history 超时，状态未知", o), &o, nil
	case "error":
		o := OutHistory{Status: StatusFailed, Note: "历史库连接失败"}
		return errResult("check_history 返回错误：历史库连接失败", o), &o, nil
	case "garbled":
		o := OutHistory{Status: StatusFailed, Note: "返回体无法解析: " + s.garbled(24)}
		return errResult("check_history 返回内容异常", o), &o, nil
	}
	out, err := CheckHistory(s.opt.Data, in, s.opt.Now)
	if err != nil {
		// 参数错误属于调用方问题，按协议错误返回，便于上层修正调用。
		return nil, nil, err
	}
	return okResult(out), &out, nil
}

// okResult 返回正常结果。文本内容给人和模型直接阅读；
// 结构化内容由 SDK 用 Out 类型自动填充，因此这里不手工写 StructuredContent，
// 避免出现"字段人为编造的 StructuredContent"与真实 Out 不一致。
func okResult(v any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
}

// errResult 返回工具级错误：IsError=true，同时带上结构化的状态，
// 这样上层既能知道"失败了"，也能知道是超时还是错误。
func errResult(msg string, v any) *mcp.CallToolResult {
	b, _ := json.MarshalIndent(v, "", "  ")
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg + "\n" + string(b)}},
	}
}

// InMemory 在进程内起一个 MCP 服务端并连一个客户端，返回已建立的 Remote。
// 供测试与"单进程演示"使用：走的仍是完整的 MCP 协议，只是传输不走网络。
// 调用方负责在结束时 Close 返回的会话；测试里用 t.Cleanup 自动关闭。
// t 可以为 nil（彩排工具不需要 *testing.T 的断言能力）。
func InMemory(t interface {
	Helper()
	Fatalf(string, ...any)
	Cleanup(func())
}, srv *Server) (*mcp.ClientSession, *Remote) {
	if t != nil {
		t.Helper()
	}
	ctx := context.Background()
	cs, ss := mcp.NewInMemoryTransports()
	if _, err := srv.MCP().Connect(ctx, ss, nil); err != nil {
		if t != nil {
			t.Fatalf("启动 MCP 服务端: %v", err)
		}
		return nil, nil
	}
	sess, err := Client(ctx, cs, &mcp.Implementation{Name: "reimburse-test", Version: "1.0.0"})
	if err != nil {
		if t != nil {
			t.Fatalf("连接 MCP 服务端: %v", err)
		}
		return nil, nil
	}
	if t != nil {
		t.Cleanup(func() { sess.Close() })
	}
	return sess, &Remote{Session: sess, Timeout: 10 * time.Second}
}
