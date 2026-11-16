package mcptools_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"reimburse/internal/mcptools"
	"reimburse/internal/schema"
)

// 跨进程链路：HTTP 挂载 MCP 服务端 -> 客户端连接 -> 调用工具。
// 这条测试证明三个工具在"真进程外、真 HTTP"下也能用，而不只是内存传输。
func TestStreamableHTTPRoundTrip(t *testing.T) {
	srv := mcptools.NewServer(mcptools.Options{Data: demoStore()})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	hs := httptest.NewServer(handler)
	defer hs.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tr := &mcp.StreamableClientTransport{Endpoint: hs.URL + "/mcp"}
	sess, err := mcptools.Client(ctx, tr, nil)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer sess.Close()

	r := &mcptools.Remote{Session: sess, Timeout: 5 * time.Second}
	if got := r.VerifyInvoice(ctx, "INV2"); got.Status != mcptools.ResultOK || len(got.Invoices) != 1 || got.Invoices[0].Status != schema.InvVoided {
		t.Fatalf("HTTP 链路查验结果错误: %+v", got)
	}
	if got := r.CheckHistory(ctx, mcptools.HistoryQuery{ApplicantID: "E1"}); got.Status != mcptools.ResultOK || len(got.History) != 2 {
		t.Fatalf("HTTP 链路查重结果错误: %+v", got)
	}
	// 只读工具列表要能看到 description，评审现场才能解释"这个工具能做什么"。
	res, err := sess.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range res.Tools {
		if !strings.Contains(tl.Description, "核验") && !strings.Contains(tl.Description, "查询") {
			t.Errorf("工具 %s 描述不足: %q", tl.Name, tl.Description)
		}
	}
}
