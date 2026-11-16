// verify-mcp 是报销审核的三个核验工具的 MCP 服务端。
//
//	go run ./cmd/verify-mcp -data data/world.json                 stdio 模式（默认，供 MCP 客户端拉起）
//	go run ./cmd/verify-mcp -data data/world.json -http 127.0.0.1:8791   Streamable HTTP（本机，可不设令牌）
//	go run ./cmd/verify-mcp -data data/world.json -http 0.0.0.0:8791     跨机访问：必须先配令牌
//	go run ./cmd/verify-mcp -data data/world.json -fault verify_invoice=timeout   注入故障
//
// 鉴权：
//   - 令牌只从环境变量 REIMBURSE_MCP_TOKENS 或 -tokens-file 读取，不用命令行参数，避免泄漏到进程列表；
//   - 监听非本机地址（0.0.0.0、局域网 IP、公网 IP）时强制要求令牌，没配就拒绝启动；
//   - 令牌只以名字和指纹出现在日志里，令牌本身不落日志、不回显。
//
// 只读服务：三个工具都不写任何数据。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"reimburse/internal/mcpauth"
	"reimburse/internal/mcptools"
	"reimburse/internal/schema"
)

func main() {
	dataPath := flag.String("data", "data/world.json", "演示数据库（world.json 或合并后的库）")
	httpAddr := flag.String("http", "", "监听地址，例如 127.0.0.1:8791；为空则用 stdio")
	faults := flag.String("fault", "", "故障注入，逗号分隔：tool=mode 或 tool:key=mode，mode 为 timeout/error/garbled")
	delay := flag.Duration("fault-delay", 3*time.Second, "timeout 故障的等待时长")
	tokensFile := flag.String("tokens-file", "", "令牌文件，每行一个（名字:令牌 或 令牌）；也可用环境变量 "+mcpauth.EnvTokens)
	requireAuth := flag.Bool("require-auth", false, "强制要求令牌（即使只监听本机）")
	auditLog := flag.String("audit-log", "", "鉴权与工具调用审计日志文件（JSON 行）；为空则不落盘")
	flag.Parse()

	store, err := loadStore(*dataPath)
	if err != nil {
		log.Fatalf("加载数据: %v", err)
	}
	fault, err := parseFaults(*faults)
	if err != nil {
		log.Fatalf("解析故障注入: %v", err)
	}
	srv := mcptools.NewServer(mcptools.Options{Data: store, Fault: fault, FaultDelay: *delay})

	if *httpAddr == "" {
		// stdio：标准输入输出即协议通道，日志必须走 stderr，否则会污染协议流。
		log.SetOutput(os.Stderr)
		fmt.Fprintf(os.Stderr, "verify-mcp 启动（stdio），数据 %s，故障 %v\n", *dataPath, fault)
		if err := srv.MCP().Run(context.Background(), &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
			log.Fatal(err)
		}
		return
	}

	// Streamable HTTP：便于现场用 curl 或浏览器开发者工具直接看请求与返回。
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil)

	// 令牌：环境变量优先，其次文件；两者都空时按"仅本机"放行，非本机监听则拒绝启动。
	tokens, tokenSource, err := mcpauth.LoadTokens(mcpauth.EnvTokens, *tokensFile)
	if err != nil {
		log.Fatalf("读取令牌: %v", err)
	}
	loopback := isLoopbackAddr(*httpAddr)
	if !loopback && len(tokens) == 0 {
		log.Fatalf("监听 %s 属于跨机访问，必须配置令牌：设置环境变量 %s 或用 -tokens-file；"+
			"仅本机演示可用 127.0.0.1", *httpAddr, mcpauth.EnvTokens)
	}
	var verifier *mcpauth.Verifier
	if len(tokens) > 0 {
		verifier, err = mcpauth.NewVerifier(tokens)
		if err != nil {
			log.Fatalf("令牌配置无效: %v", err)
		}
	} else if *requireAuth {
		log.Fatalf("-require-auth 已开启，但没有配置任何令牌")
	}

	mux := http.NewServeMux()
	// /healthz 不鉴权：只回"服务活着"与工具名，不含任何业务数据。
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "auth": verifier != nil, "tools": []string{
			mcptools.ToolVerifyInvoice, mcptools.ToolCheckCompany, mcptools.ToolCheckHistory,
		}})
	})
	if verifier != nil {
		log.Printf("鉴权已启用，令牌 %v（%s）", verifier.Names(), tokenSource)
		// 顺序很重要：审计在外层，鉴权在内层。
		// 本包的鉴权中间件会把令牌信息原地写回同一个 request，所以外层能读到调用方名。
		mux.Handle("/mcp", auditLogMiddleware(*auditLog, verifier.Middleware()(handler)))
	} else {
		log.Printf("警告：未配置令牌，服务不校验调用方；确认监听地址 %s 只允许本机访问", *httpAddr)
		mux.Handle("/mcp", auditLogMiddleware(*auditLog, handler))
	}
	hs := &http.Server{Addr: *httpAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes: 1 << 20}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Printf("verify-mcp 启动（HTTP）http://%s/mcp，数据 %s，故障 %v", *httpAddr, *dataPath, fault)
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = hs.Shutdown(shutdown)
}

// loadStore 支持两种数据文件：
//   - world.json：单条用例的库（与测试集一致）；
//   - 目录：把目录下所有 world.json 合并，供演示时跨单据查重。
func loadStore(path string) (*mcptools.Store, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var w schema.World
		if err := json.Unmarshal(b, &w); err != nil {
			return nil, err
		}
		return mcptools.NewStore(w), nil
	}
	var w schema.World
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(path, e.Name()))
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

// mergeWorlds 按主键去重合并，避免同一家企业因出现在多条用例里而重复返回。
func mergeWorlds(a, b schema.World) schema.World {
	seenInv, seenCo, seenHist := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range a.Invoices {
		seenInv[r.InvoiceNo] = true
	}
	for _, c := range a.Companies {
		seenCo[c.TaxID] = true
	}
	for _, h := range a.History {
		seenHist[h.ClaimNo] = true
	}
	for _, r := range b.Invoices {
		if !seenInv[r.InvoiceNo] {
			seenInv[r.InvoiceNo] = true
			a.Invoices = append(a.Invoices, r)
		}
	}
	for _, c := range b.Companies {
		if !seenCo[c.TaxID] {
			seenCo[c.TaxID] = true
			a.Companies = append(a.Companies, c)
		}
	}
	for _, h := range b.History {
		if !seenHist[h.ClaimNo] {
			seenHist[h.ClaimNo] = true
			a.History = append(a.History, h)
		}
	}
	return a
}

// parseFaults 解析 "tool=mode" 与 "tool:key=mode"。
func parseFaults(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("格式应为 tool=mode 或 tool:key=mode，收到 %q", item)
		}
		k, v := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		switch v {
		case "timeout", "error", "garbled":
		default:
			return nil, fmt.Errorf("未知故障方式 %q，只支持 timeout/error/garbled", v)
		}
		base := strings.SplitN(k, ":", 2)[0]
		switch base {
		case mcptools.ToolVerifyInvoice, mcptools.ToolCheckCompany, mcptools.ToolCheckHistory:
		default:
			return nil, fmt.Errorf("未知工具 %q", base)
		}
		out[k] = v
	}
	return out, nil
}
