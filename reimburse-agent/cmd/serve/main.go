// serve 启动单页审核界面。
//
//	go run ./cmd/serve
//	go run ./cmd/serve -addr 127.0.0.1:8790 -mcp-url http://127.0.0.1:8791/mcp
//	go run ./cmd/serve -addr 0.0.0.0:8790 -ui-token-file tokens.txt   跨机访问界面
//
// 界面本身只读业务数据、不做判定；判定全部在服务端的规则引擎里。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"reimburse/internal/identity"
	"reimburse/internal/llm"
	"reimburse/internal/mcpauth"
	"reimburse/internal/quota"
	"reimburse/internal/server"
	"reimburse/internal/task"
	"reimburse/internal/upload"
	"reimburse/plugins/reimburse-audit/policy"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8790", "监听地址；跨机访问用 0.0.0.0:8790 并配令牌")
	data := flag.String("data", "data", "测试集根目录（含 cases/）")
	cfgPath := flag.String("config", "", "模型配置文件，缺省用内置默认值")
	rulesPath := flag.String("rules", "", "外部规则表，缺省用内嵌规则表")
	mcpURL := flag.String("mcp-url", "", "连接独立 MCP 服务；为空则在进程内起 MCP 服务")
	mcpData := flag.String("mcp-data", "", "进程内 MCP 服务的数据文件；为空则用当前单据的库")
	uiToken := flag.String("ui-token-file", "", "界面访问令牌文件；监听非本机地址时必须提供")
	replayDir := flag.String("replay", "data/replay", "录制回放目录：实时调用失败时用它兜底；不存在则忽略")
	localOp := flag.String("local-operator", "", "本机（127.0.0.1，无令牌）访问时绑定的工号；须在人员名单里。留空则本机复核也被拒绝")
	peoplePath := flag.String("people", "people.json", "人员名单（工号 + 姓名 + 权限）。缺省文件不存在时：拒绝一切复核写入")
	quotaPath := flag.String("quota-file", "data/quota.json", "用量配额文件（落盘，跨重启保留）")
	weeklyBudget := flag.Float64("weekly-budget", 50, "周预算（美元）。0 表示不限制")
	perCaseAlert := flag.Float64("per-case-alert", 0.05, "单张单据成本告警线（美元）")
	seedUsage := flag.Float64("seed-usage", 0, "演示用：预置本周已用金额（美元），用于一键演示熔断")
	seedClient := flag.String("seed-client", "reviewer", "演示用：预置用量记到哪个调用方名下")
	uploadDir := flag.String("upload-dir", "", "真实票据上传目录；默认 <data>/uploads。空字符串表示关闭上传")
	reviewFile := flag.String("review-file", "data/reviews.json", "复核记录文件（落盘，跨重启保留）。空字符串表示只在内存")
	taskFile := flag.String("task-file", "data/tasks.json", "任务库文件（落盘，跨重启保留）。空字符串表示不启用任务流")
	demo := flag.Bool("demo-synthetic", false, "无密钥演示模式：模型不可用时用合成抽取，界面会标明「合成」")
	flag.Parse()

	rf, err := loadRules(*rulesPath)
	if err != nil {
		log.Fatalf("读取规则表: %v", err)
	}
	cfg, err := llm.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("读取模型配置: %v", err)
	}
	srv, err := server.New(*data, rf, cfg)
	if err != nil {
		log.Fatalf("加载测试集: %v", err)
	}
	srv.MCPURL = *mcpURL
	srv.MCPData = *mcpData

	// 人员名单：文件不存在时是空名单（拒绝一切复核写入），不是"放行所有人"。
	people, err := identity.LoadFile(*peoplePath)
	if err != nil {
		log.Fatalf("读取人员名单: %v", err)
	}
	srv.People = people
	if people.Size() == 0 {
		log.Printf("警告：人员名单为空（%s）。复核结论将被拒绝写入——这是默认拒绝，不是故障。", *peoplePath)
	} else {
		log.Printf("人员名单 %d 人（%s）", people.Size(), *peoplePath)
	}

	// 配额：落盘，跨重启保留。
	qcfg := quota.DefaultConfig()
	qcfg.WeeklyBudgetUSD = *weeklyBudget
	if *perCaseAlert > 0 {
		qcfg.PerCaseAlertUSD = *perCaseAlert
	}
	q, err := quota.Open(*quotaPath, qcfg)
	if err != nil {
		log.Fatalf("打开配额文件: %v", err)
	}
	srv.Quota = q
	if *weeklyBudget > 0 {
		log.Printf("周预算 $%.2f，用量文件 %s（当前周 %s 已用 $%.4f）",
			*weeklyBudget, *quotaPath, quota.WeekKey(time.Now()), q.Check("").UsedUSD)
	} else {
		log.Printf("未设置周预算（不限），用量文件 %s", *quotaPath)
	}
	if *reviewFile != "" {
		if err := srv.OpenReviewStore(*reviewFile); err != nil {
			log.Fatalf("打开复核记录: %v", err)
		}
		log.Printf("复核记录 %s（已有 %d 条，含已撤销）", *reviewFile, len(srv.Reviews()))
	}
	if *taskFile != "" {
		tk, err := task.Open(*taskFile)
		if err != nil {
			log.Fatalf("打开任务库: %v", err)
		}
		srv.Tasks = tk
		log.Printf("任务库 %s（已有 %d 张单）", *taskFile, len(tk.List()))
	}
	if *seedUsage > 0 {
		st := q.AddUsage(*seedClient, *seedUsage)
		log.Printf("演示预置：给 %s 记入 $%.2f，本周累计 $%.4f（%.1f%%）",
			*seedClient, *seedUsage, st.UsedUSD, st.Ratio*100)
	}
	if st, err := os.Stat(*replayDir); err == nil && st.IsDir() {
		srv.ReplayDir = *replayDir
		log.Printf("回放目录 %s 已就绪：优先用真实录制，没有录制才用合成", *replayDir)
	}
	// 真实票据上传：默认开，落盘到 <data>/uploads。
	// 传 -upload-dir - 可关闭（例如纯演示、不允许写入文件的场合）。
	// 上传的单据没有 truth.json，所以 -demo-synthetic 下会被拒绝审——
	// 启动时已提示，不会到现场才发现。
	if *uploadDir != "-" {
		root := *data
		up := &upload.Processor{Root: root}
		srv.Uploader = up
		log.Printf("票据上传已启用，落盘到 %s", filepath.Join(root, "uploads"))
		if *demo {
			log.Printf("注意：当前是合成演示模式，上传的真实票据无法审核" +
				"（没有标准答案）。要审真实票据请配模型密钥并去掉 -demo-synthetic")
		}
	}
	srv.DemoSynthetic = *demo
	if *demo {
		// 合成模式靠标准答案回放字段。没有标准答案的单据跑不了，
		// 与其等现场点开才发现，不如启动时就报清楚是哪几条。
		var noTruth []string
		for _, c := range srv.Cases {
			if !c.HasTruth {
				noTruth = append(noTruth, c.ID)
			}
		}
		if len(noTruth) > 0 {
			log.Fatalf("以下单据没有 truth.json，无法用于合成演示模式：%v\n"+
				"合成模式只能跑带标准答案的测试集；要审真实票据请去掉 -demo-synthetic 并配置模型密钥", noTruth)
		}
		log.Printf("无密钥演示模式已开启：抽取结果为合成数据，不是模型真实输出，界面与报告都会标注")
	}
	if *mcpURL != "" {
		tok, _, err := mcpauth.LoadClientToken(mcpauth.EnvClientToken, *uiToken)
		if err != nil {
			log.Fatalf("连接 MCP 服务需要令牌: %v", err)
		}
		srv.MCPToken = tok
	}

	// 本机操作人：只有在监听回环地址时才有意义。
	if *localOp != "" {
		if isLoopback(*addr) {
			p, err := people.Authenticate(*localOp, "review")
			if err != nil {
				log.Fatalf("本机操作人 %q 不可用: %v", *localOp, err)
			}
			srv.LocalOperator = p.EmployeeID
			log.Printf("本机操作人绑定为 %s（%s）——仅 127.0.0.1 直连生效，跨机仍需令牌", p.EmployeeID, p.Name)
		} else {
			log.Printf("忽略 -local-operator：监听 %s 属于跨机访问，身份一律以令牌绑定为准", *addr)
		}
	}

	handler := srv.Routes()
	// 界面跨机访问：与 MCP 服务同一套令牌机制，避免"工具锁了、界面没锁"。
	if !isLoopback(*addr) {
		toks, src, err := mcpauth.LoadTokens(mcpauth.EnvUITokens, *uiToken)
		if err != nil || len(toks) == 0 {
			log.Fatalf("监听 %s 属于跨机访问，必须给界面配令牌：设置环境变量 %s 或用 -ui-token-file",
				*addr, mcpauth.EnvUITokens)
		}
		v, err := mcpauth.NewVerifier(toks)
		if err != nil {
			log.Fatalf("界面令牌配置无效: %v", err)
		}
		log.Printf("界面鉴权已启用，令牌 %v（%s）", v.Names(), src)
		handler = uiAuth(v, handler)
	}

	hs := &http.Server{Addr: *addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Printf("审核界面 http://%s ，测试集 %s，规则 %s，MCP %s", *addr, *data,
			rf.Policy.Version, toolsMode(srv.MCPURL))
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = hs.Shutdown(shut)
}

// uiAuth 在令牌中间件前面补一层"给浏览器用的登录页"。
//
// 纯 401 对现场太不友好：浏览器只会把 "no bearer token" 渲染成一行白底黑字，
// 没人知道该干什么。这里让未携带令牌的页面请求返回一个只需要粘贴令牌的登录框，
// 校验通过后种一个 HttpOnly Cookie，之后页面与 SSE 都自动带上。
//
// 安全上的取舍：
//   - Cookie 是 HttpOnly + SameSite=Strict，前端脚本读不到；
//   - 仍然支持 Authorization: Bearer，供脚本与 curl 使用；
//   - 登录失败只回一句"令牌不正确"，不暴露令牌名或指纹。
func uiAuth(v *mcpauth.Verifier, next http.Handler) http.Handler {
	guarded := v.Middleware()(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			serveLogin(v, guarded, w, r)
			return
		}
		if r.Header.Get("Authorization") == "" {
			if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
				r.Header.Set("Authorization", "Bearer "+c.Value)
			}
		}
		// 浏览器打开页面但没带任何凭证：给登录页，而不是一行英文报错。
		if r.URL.Path == "/" && r.Header.Get("Authorization") == "" && acceptsHTML(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		guarded.ServeHTTP(w, r)
	})
}

const cookieName = "reimburse_ui"

func acceptsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// serveLogin 处理登录页与登录提交。
func serveLogin(v *mcpauth.Verifier, guarded http.Handler, w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		tok := strings.TrimSpace(r.FormValue("token"))
		if tok == "" {
			writeLoginPage(w, "请输入令牌", http.StatusOK)
			return
		}
		// 复用同一套校验：能通过 MCP 中间件的令牌，就是能用的令牌。
		probe := r.Clone(r.Context())
		probe.Header = http.Header{}
		probe.Header.Set("Authorization", "Bearer "+tok)
		rec := &probeWriter{header: http.Header{}}
		guarded.ServeHTTP(rec, probe)
		if rec.status == http.StatusUnauthorized {
			writeLoginPage(w, "令牌不正确", http.StatusOK)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 12 * 3600})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	writeLoginPage(w, "", http.StatusOK)
}

// probeWriter 用于内部试打一次请求，只关心状态码。
type probeWriter struct {
	header http.Header
	status int
}

func (p *probeWriter) Header() http.Header { return p.header }
func (p *probeWriter) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = 200
	}
	return len(b), nil
}
func (p *probeWriter) WriteHeader(code int) {
	if p.status == 0 {
		p.status = code
	}
}

func writeLoginPage(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	tip := ""
	if msg != "" {
		tip = `<p class="err">` + msg + `</p>`
	}
	_, _ = w.Write([]byte(loginHTML(tip)))
}

func loginHTML(tip string) string {
	return `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>报销审核 · 登录</title>
<style>
body{margin:0;height:100vh;display:grid;place-items:center;background:#0f1115;color:#e6e9ef;
font:14px/1.6 -apple-system,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif}
.card{width:340px;background:#171a21;border:1px solid #2a2f3a;border-radius:12px;padding:24px}
h1{font-size:15px;margin:0 0 4px}p.sub{color:#6b7484;font-size:12px;margin:0 0 16px}
input{width:100%;box-sizing:border-box;background:#1e222b;border:1px solid #2a2f3a;color:#e6e9ef;
border-radius:8px;padding:9px 11px;font-size:13px;font-family:ui-monospace,Consolas,monospace}
button{width:100%;margin-top:12px;background:#1f6feb;border:none;color:#fff;padding:9px;border-radius:8px;
font-size:13px;font-weight:600;cursor:pointer}
.err{color:#f85149;font-size:12px;margin:8px 0 0}
.tip{color:#8b94a5;font-size:11.5px;margin-top:14px;line-height:1.7}
</style></head><body><div class="card">
<h1>报销审核 · AI 同事</h1>
<p class="sub">本服务已开启访问控制，请粘贴访问令牌</p>
<form method="post" action="/login">
<input name="token" type="password" placeholder="粘贴访问令牌" autofocus autocomplete="off">
<button type="submit">进入</button>
</form>` + tip + `
<p class="tip">令牌由管理员通过环境变量 REIMBURSE_UI_TOKENS 配置。<br>脚本与 curl 可直接用请求头 Authorization: Bearer &lt;令牌&gt;。</p>
</div></body></html>`
}

func toolsMode(u string) string {
	if u == "" {
		return "进程内"
	}
	return "远程 " + u
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch strings.ToLower(host) {
	case "", "0.0.0.0", "::", "[::]":
		return false
	case "localhost":
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func loadRules(p string) (*policy.File, error) {
	if p == "" {
		return policy.Load()
	}
	return policy.LoadFile(p)
}

var _ = fmt.Sprintf
