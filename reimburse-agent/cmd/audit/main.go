// audit 用真实链路审核一张单据：双模型抽取 → 本地工具核验 → 规则判定 → 输出报告。
//
//	go run ./cmd/audit -case data/cases/C01                     真实调用 DeepSeek 与 Claude
//	go run ./cmd/audit -case data/cases/C01 -fault verify_invoice=timeout   注入工具故障
//	go run ./cmd/audit -case data/cases/C01 -disable claude     模拟一个模型不可用
//	go run ./cmd/audit -case data/cases/C01 -explain deepseek   额外让模型润色审核意见
//
// 退出码：0 表示审核跑完（不论结论）；2 表示参数或环境有问题（例如没配密钥）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reimburse/internal/audit"
	"reimburse/internal/llm"
	"reimburse/internal/mcpauth"
	"reimburse/internal/schema"
	"reimburse/rules"
)

func main() {
	casePath := flag.String("case", "", "用例目录，例如 data/cases/C01")
	invoice := flag.String("invoice", "", "发票文件路径（与 -claim 一起用时生效）")
	support := flag.String("support", "", "佐证凭证文件路径，可空")
	claimPath := flag.String("claim", "", "报销单 JSON 路径（脱离用例集使用）")
	cfgPath := flag.String("config", "", "模型配置文件，缺省用内置默认值")
	rulesPath := flag.String("rules", "", "外部规则表，缺省用内嵌规则表")
	out := flag.String("out", "", "把完整报告写到该 JSON 文件")
	explain := flag.String("explain", "", "用哪个模型润色审核意见：deepseek / claude，空表示不调用模型")
	disable := flag.String("disable", "", "模拟不可用的模型，逗号分隔：deepseek,claude")
	fault := flag.String("fault", "", "工具故障注入，逗号分隔：verify_invoice=timeout,check_company=error")
	jsonOut := flag.Bool("json", false, "只输出报告 JSON，便于脚本处理")
	replayDir := flag.String("replay", "", "录制回放目录；实时调用不可用时改用该目录下的录制")
	recordDir := flag.String("record", "", "把本次真实调用的响应录制到该目录，供日后回放")
	mcpURL := flag.String("mcp-url", "", "连接独立的 MCP 服务，例如 http://127.0.0.1:8791/mcp；为空则在进程内启动 MCP 服务")
	mcpData := flag.String("mcp-data", "", "进程内 MCP 服务的数据文件，缺省为 data/world.json")
	tokenFile := flag.String("token-file", "", "MCP 服务端令牌文件（取第一条）；也可用环境变量 "+mcpauth.EnvClientToken)
	_ = recordDir
	flag.Parse()

	rf, err := loadRules(*rulesPath)
	if err != nil {
		fatal("读取规则表: %v", err)
	}
	cfg, err := llm.LoadConfig(*cfgPath)
	if err != nil {
		fatal("读取模型配置: %v", err)
	}

	var claim *schema.Claim
	var invoiceText, supportText string
	if *casePath != "" {
		c, err := schema.LoadCase(*casePath)
		if err != nil {
			fatal("读取用例: %v", err)
		}
		claim, invoiceText, supportText = &c.Claim, c.InvoiceText, c.SupportText
	} else if *claimPath != "" && *invoice != "" {
		b, err := os.ReadFile(*claimPath)
		if err != nil {
			fatal("读取报销单: %v", err)
		}
		claim = &schema.Claim{}
		if err := json.Unmarshal(b, claim); err != nil {
			fatal("解析报销单: %v", err)
		}
		ib, err := os.ReadFile(*invoice)
		if err != nil {
			fatal("读取发票: %v", err)
		}
		invoiceText = string(ib)
		if *support != "" {
			sb, err := os.ReadFile(*support)
			if err != nil {
				fatal("读取佐证凭证: %v", err)
			}
			supportText = string(sb)
		}
	} else {
		fatal("需要 -case，或同时提供 -claim 与 -invoice")
	}

	client := llm.NewClient(&cfg)
	rv := &audit.Reviewer{LLM: client, Client: client, Cfg: &cfg, Disabled: map[string]bool{}}
	if *replayDir != "" && *casePath != "" {
		rv.Replay = &llm.Replayer{Dir: *replayDir}
		rv.CaseID = filepath.Base(*casePath)
	}
	for _, d := range splitList(*disable) {
		rv.Disabled[d] = true
	}
	opt := audit.Options{Explain: *explain, Injected: map[string]audit.ToolStatus{}}
	for _, f := range splitList(*fault) {
		parts := strings.SplitN(f, "=", 2)
		if len(parts) != 2 {
			fatal("故障注入格式应为 工具=状态，收到 %q", f)
		}
		switch parts[1] {
		case "timeout":
			opt.Injected[parts[0]] = audit.ToolTimeout
		case "error", "failed":
			opt.Injected[parts[0]] = audit.ToolFailed
		default:
			fatal("未知故障状态 %q", parts[1])
		}
	}

	// 工具层走 MCP：默认把 MCP 服务端跑在本进程内（仍是完整的 MCP 客户端/服务端协议），
	// 也可以用 -mcp-url 连到独立进程或远程服务。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// 只有连远程服务时才需要令牌；进程内是自己起的服务，不经过网络。
	token := ""
	if *mcpURL != "" {
		var src string
		token, src, err = mcpauth.LoadClientToken(mcpauth.EnvClientToken, *tokenFile)
		if err != nil {
			fatal("%v", err)
		}
		if !*jsonOut {
			fmt.Printf("MCP 服务 %s，令牌来自%s（指纹 %s）\n", *mcpURL, src, mcpauth.Fingerprint(token))
		}
	}
	ex, closer, err := connectTools(ctx, *mcpURL, *mcpData, *casePath, *fault, token)
	if err != nil {
		fatal("连接 MCP 工具: %v", err)
	}
	defer closer()

	if !*jsonOut {
		fmt.Println("模型接入：")
		fmt.Println("  DeepSeek  " + cfg.DeepSeek.Redacted())
		fmt.Println("  Claude    " + cfg.Claude.Redacted())
		fmt.Println("规则表：", rf.Policy.Name, rf.Policy.Version, "，规则", len(rf.Rules), "条，判定项", len(rf.AllCheckIDs()), "个")
		fmt.Println()
	}

	eng := audit.New(rf)
	var onEvent audit.Emitter
	if !*jsonOut {
		onEvent = func(ev audit.Event) {
			fmt.Printf("[%4d] %-9s %-11s %s", ev.Seq, ev.Stage, ev.Kind, ev.Title)
			if ev.Detail != "" {
				fmt.Printf(" — %s", oneLine(ev.Detail))
			}
			fmt.Println()
		}
	}
	opt.OnEvent = onEvent
	rep := eng.Run(ctx, claim, invoiceText, supportText, ex, rv, opt)

	if *out != "" {
		if err := schema.WriteJSON(*out, rep); err != nil {
			fatal("写报告: %v", err)
		}
	}
	if len(rv.Recorded) > 0 {
		fmt.Printf("注意：%s 使用了录制回放，不是本次实时输出。\n", strings.Join(rv.Recorded, "、"))
	}
	if *jsonOut {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
		return
	}
	printReport(rep)
}

func printReport(rep *audit.Report) {
	fmt.Println()
	fmt.Println("========== 审核结论 ==========")
	fmt.Printf("结论：%s\n", outcomeCN(rep.Result.Decision))
	fmt.Printf("核准金额：%s\n", yuan(rep.Result.ApprovedFen))
	fmt.Printf("命中：%v\n", rep.Result.Hits)
	for _, w := range rep.Warnings {
		fmt.Println("降级说明：", w)
	}
	fmt.Printf("耗时 %dms，估算成本 $%.5f\n", rep.ElapsedMS, rep.CostUSD)
	fmt.Println()
	fmt.Println("========== 审核意见 ==========")
	fmt.Println(rep.Opinion)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " / ")
	if len([]rune(s)) > 160 {
		return string([]rune(s)[:160]) + "…"
	}
	return s
}

func yuan(fen int64) string {
	return fmt.Sprintf("¥%d.%02d", fen/100, fen%100)
}

func outcomeCN(o string) string {
	switch o {
	case rules.Approve:
		return "通过"
	case rules.ApproveAdj:
		return "调整后通过"
	case rules.Reject:
		return "驳回"
	case rules.ManualReview:
		return "转人工复核"
	case rules.NeedMoreInfo:
		return "退回补材料"
	}
	return o
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func loadRules(path string) (*rules.File, error) {
	if path == "" {
		return rules.Load()
	}
	return rules.LoadFile(path)
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(2)
}

var _ = json.Marshal
