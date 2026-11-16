package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"reimburse/internal/audit"
	"reimburse/internal/mcpauth"
	"reimburse/internal/mcptools"
	"reimburse/internal/schema"
)

// connectTools 建立 MCP 工具连接。
//
// 三种模式：
//  1. -mcp-url 给了地址：连远程/独立进程，演示时最能说明"工具在进程外"；
//  2. 进程内服务端 + 内存传输：默认，一条命令即可跑，但走的仍是完整的 MCP 协议
//     （工具发现、入参 schema、结构化返回都一样）；
//  3. -mcp-data 指向文件或目录，决定服务端看到的库。
//
// 返回的 closer 必须调用，否则会话与 goroutine 会泄漏。
func connectTools(ctx context.Context, url, dataPath, casePath, fault, token string) (audit.Explorer, func(), error) {
	var sess *mcp.ClientSession
	var err error
	if url != "" {
		// 令牌随每个请求发送；MCP 客户端重连、建立 SSE 长连接时同样带着它。
		tr := mcpauth.ClientTransport(url, mcpauth.StaticToken(token), 60*time.Second)
		sess, err = mcptools.Client(ctx, tr, &mcp.Implementation{Name: "reimburse-audit-cli", Version: "1.0.0"})
		if err != nil {
			// 鉴权失败时给出可操作的提示，而不是只抛 "connecting: 401"。
			if strings.Contains(err.Error(), "401") ||
				strings.Contains(strings.ToLower(err.Error()), "unauthorized") {
				return nil, func() {}, fmt.Errorf("连接 %s 被拒绝（401）：令牌缺失或不匹配。"+
					"请设置环境变量 %s，或用 -token-file 指定令牌文件", url, mcpauth.EnvClientToken)
			}
			return nil, func() {}, fmt.Errorf("连接 %s: %w", url, err)
		}
	} else {
		// 进程内：把世界数据装进 Store，起一个真实 MCP Server，再用内存传输连上去。
		store, err := loadMCPSource(dataPath, casePath)
		if err != nil {
			return nil, func() {}, err
		}
		fl, err := parseFaultSpec(fault)
		if err != nil {
			return nil, func() {}, err
		}
		srv := mcptools.NewServer(mcptools.Options{Data: store, Fault: fl, FaultDelay: 800 * time.Millisecond})
		c, s := mcp.NewInMemoryTransports()
		if _, err := srv.MCP().Connect(ctx, s, nil); err != nil {
			return nil, func() {}, err
		}
		sess, err = mcptools.Client(ctx, c, &mcp.Implementation{Name: "reimburse-audit-cli", Version: "1.0.0"})
		if err != nil {
			return nil, func() {}, err
		}
	}

	// 启动自检：三个工具必须都能被发现，缺一个就直接失败，不能带着残缺的工具集去审核。
	names, err := (&mcptools.Remote{Session: sess}).ListTools(ctx)
	if err != nil {
		sess.Close()
		return nil, func() {}, fmt.Errorf("列出工具: %w", err)
	}
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	for _, n := range []string{mcptools.ToolVerifyInvoice, mcptools.ToolCheckCompany, mcptools.ToolCheckHistory} {
		if !have[n] {
			sess.Close()
			return nil, func() {}, fmt.Errorf("MCP 服务缺少工具 %s（已发现 %v）", n, names)
		}
	}

	remote := &mcptools.Remote{Session: sess, Timeout: 10 * time.Second}
	spy := &audit.RemoteSpy{Inner: audit.WrapMCP(remote)}
	return spy, func() { sess.Close() }, nil
}

// loadMCPSource 优先用 -mcp-data；没给则用当前用例目录的 world.json（保证单据与库自洽）。
func loadMCPSource(dataPath, casePath string) (*mcptools.Store, error) {
	p := dataPath
	if p == "" {
		if casePath != "" {
			p = filepath.Join(casePath, "world.json")
		} else {
			p = filepath.Join("data", "world.json")
		}
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil, fmt.Errorf("读取工具数据 %s: %w", p, err)
	}
	if !st.IsDir() {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var w schema.World
		if err := json.Unmarshal(b, &w); err != nil {
			return nil, err
		}
		return mcptools.NewStore(w), nil
	}
	// 目录：合并全部 world.json，用于跨单据查重的演示。
	var w schema.World
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(p, e.Name()))
		if err != nil {
			return nil, err
		}
		var one schema.World
		if err := json.Unmarshal(b, &one); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		w = mergeWorlds(w, one)
	}
	return mcptools.NewStore(w), nil
}

// mergeWorlds 按主键去重合并。
func mergeWorlds(a, b schema.World) schema.World {
	si, sc, sh := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range a.Invoices {
		si[r.InvoiceNo] = true
	}
	for _, c := range a.Companies {
		sc[c.TaxID] = true
	}
	for _, h := range a.History {
		sh[h.ClaimNo] = true
	}
	for _, r := range b.Invoices {
		if !si[r.InvoiceNo] {
			si[r.InvoiceNo] = true
			a.Invoices = append(a.Invoices, r)
		}
	}
	for _, c := range b.Companies {
		if !sc[c.TaxID] {
			sc[c.TaxID] = true
			a.Companies = append(a.Companies, c)
		}
	}
	for _, h := range b.History {
		if !sh[h.ClaimNo] {
			sh[h.ClaimNo] = true
			a.History = append(a.History, h)
		}
	}
	return a
}

// parseFaultSpec 解析 "tool=mode" / "tool:key=mode"，直接作为 MCP 服务端的故障注入。
func parseFaultSpec(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("故障注入格式应为 工具=方式 或 工具:键=方式，收到 %q", item)
		}
		k, v := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		switch v {
		case "timeout", "error", "failed", "garbled":
		default:
			return nil, fmt.Errorf("未知故障方式 %q，只支持 timeout/error/garbled", v)
		}
		out[k] = v
	}
	return out, nil
}
