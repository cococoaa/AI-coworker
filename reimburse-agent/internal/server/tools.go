package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"reimburse/internal/audit"
	"reimburse/internal/mcpauth"
	"reimburse/internal/mcptools"
	"reimburse/internal/schema"
)

// connect 建立 MCP 工具连接。
//
// 与命令行同一套逻辑：给了 -mcp-url 就连远程（需要令牌），否则在进程内起一个
// 真实的 MCP 服务端并用内存传输连上——走的仍是完整 MCP 协议。
//
// extra 是人工复核回写的历史记录，合并进查重库，让"人纠正过的结论"影响后续审核。
func connect(ctx context.Context, url, dataPath string, c *schema.Case, token string, extra []schema.HistRecord) (audit.Explorer, func(), error) {
	if url != "" {
		tr := mcpauth.ClientTransport(url, mcpauth.StaticToken(token), 60*time.Second)
		sess, err := mcptools.Client(ctx, tr, &mcp.Implementation{Name: "reimburse-web", Version: "1.0.0"})
		if err != nil {
			if strings.Contains(err.Error(), "401") {
				return nil, func() {}, fmt.Errorf("连接 %s 被拒绝（401）：令牌缺失或不匹配", url)
			}
			return nil, func() {}, err
		}
		return audit.WrapMCP(&mcptools.Remote{Session: sess, Timeout: 10 * time.Second}), func() { sess.Close() }, nil
	}

	// 进程内：以当前用例的 world 为底，叠加复核回写。
	w := c.World
	w.History = append(append([]schema.HistRecord{}, w.History...), extra...)
	if dataPath != "" {
		// 指定了独立的库（跨单据查重演示）时，改用该库的公司与发票，历史仍以合并结果为准。
		loaded, err := loadWorldFile(dataPath)
		if err != nil {
			return nil, func() {}, err
		}
		loaded.History = append(loaded.History, w.History...)
		w = loaded
		w.Faults = c.World.Faults
	}

	fl := map[string]string{}
	for _, f := range c.World.Faults {
		fl[f.Tool+":"+f.Key] = f.Mode
	}
	srv := mcptools.NewServer(mcptools.Options{Data: mcptools.NewStore(w), Fault: fl, FaultDelay: 800 * time.Millisecond})
	cs, ss := mcp.NewInMemoryTransports()
	if _, err := srv.MCP().Connect(ctx, ss, nil); err != nil {
		return nil, func() {}, err
	}
	sess, err := mcptools.Client(ctx, cs, &mcp.Implementation{Name: "reimburse-web", Version: "1.0.0"})
	if err != nil {
		return nil, func() {}, err
	}
	return audit.WrapMCP(&mcptools.Remote{Session: sess, Timeout: 10 * time.Second}), func() { sess.Close() }, nil
}

func loadWorldFile(path string) (schema.World, error) {
	var w schema.World
	b, err := os.ReadFile(path)
	if err != nil {
		return w, err
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return w, err
	}
	return w, nil
}
