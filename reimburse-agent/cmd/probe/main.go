package main

// 一次性的连通性探测：确认指定供应商（含自定义 BaseURL）能否真实抽取，并打印原始用量。
// 令牌只从环境变量注入，不写入任何文件、不打印。
//
//	PROBE_TOKEN=... go run ./cmd/probe -provider claude -baseurl https://api.deepseek.com/anthropic -model deepseek-flash

import (
	"context"
	"flag"
	"fmt"
	"os"

	"reimburse/internal/llm"
	"reimburse/internal/schema"
)

func main() {
	provider := flag.String("provider", "claude", "deepseek / claude")
	base := flag.String("baseurl", "", "覆盖 BaseURL（例如 deepseek 的 anthropic 兼容端点）")
	model := flag.String("model", "", "覆盖模型名")
	caseID := flag.String("case", "C01", "用例编号")
	cfgPath := flag.String("config", "", "模型配置文件（读取 base_url / model / user_agent）")
	flag.Parse()

	if os.Getenv("PROBE_TOKEN") == "" {
		fmt.Println("缺少 PROBE_TOKEN")
		os.Exit(2)
	}
	cfg := llm.DefaultConfig()
	ep, ok := cfg.Endpoint(*provider)
	if !ok {
		fmt.Println("未知供应商:", *provider)
		os.Exit(2)
	}
	ep.APIKeyEnv = "PROBE_TOKEN"
	if *base != "" {
		ep.BaseURL = *base
	}
	if *model != "" {
		ep.Model = *model
	}
	if *cfgPath != "" {
		loaded, err := llm.LoadConfig(*cfgPath)
		if err != nil {
			fmt.Println("读取配置:", err)
			os.Exit(2)
		}
		cfg = loaded
		ep, _ = cfg.Endpoint(*provider)
		ep.APIKeyEnv = "PROBE_TOKEN"
		if *base != "" {
			ep.BaseURL = *base
		}
		if *model != "" {
			ep.Model = *model
		}
	}
	ep.TimeoutSec, ep.MaxRetries = 60, 0
	if err := cfg.Normalize(); err != nil {
		fmt.Println("配置错误:", err)
		os.Exit(2)
	}

	c, err := schema.LoadCase("data/cases/" + *caseID)
	if err != nil {
		fmt.Println("读取用例:", err)
		os.Exit(2)
	}
	src := llm.Source{Category: c.Claim.Category, InvoiceText: c.InvoiceText, SupportText: c.SupportText}
	client := llm.NewClient(&cfg)

	// 先打原始调用，拿到真实 usage，再解析字段——这样能把"模型问题"和"解析问题"分开。
	system, user, sch := llm.ExtractPrompt(src)
	raw := client.Call(context.Background(), *provider, &llm.Request{System: system,
		Messages: []llm.Message{{Role: "user", Content: user}}, Schema: sch, SchemaName: "invoice_extraction"})
	if raw.Err != nil {
		fmt.Printf("调用失败：%v\n", raw.Err)
		os.Exit(1)
	}
	fmt.Printf("原始调用：model=%q usage=%+v latency=%dms attempts=%d\n",
		raw.Resp.Model, raw.Resp.Usage, raw.Resp.LatencyMS, raw.Resp.Attempts)

	ex, issue := client.ExtractWith(context.Background(), *provider, src, nil)
	if issue != nil {
		fmt.Printf("抽取失败：%s / %s\n", issue.Kind, issue.Detail)
		os.Exit(1)
	}
	fmt.Printf("抽取结果 usage=%+v\n", ex.Usage)
	t := c.Truth.Invoice
	checks := []struct {
		name, got, want string
	}{
		{"发票号", ex.Fields[llm.FInvoiceNo].Value, t.InvoiceNo},
		{"开票日", ex.Fields[llm.FIssueDate].Value, t.IssueDate},
		{"金额", ex.Fields[llm.FTotalAmount].Value, fmt.Sprintf("%d.%02d", t.TotalFen/100, t.TotalFen%100)},
		{"购买方税号", ex.Fields[llm.FBuyerTaxID].Value, t.BuyerTaxID},
		{"销售方税号", ex.Fields[llm.FSellerTaxID].Value, t.SellerTaxID},
		{"发票项目", ex.Fields[llm.FItemName].Value, t.ItemName},
	}
	bad := 0
	for _, ck := range checks {
		mark := "ok"
		if ck.got != ck.want {
			mark = "DIFF"
			bad++
		}
		fmt.Printf("  %-8s %-6s got=%q want=%q\n", ck.name, mark, ck.got, ck.want)
	}
	fmt.Printf("字段一致 %d/%d\n", len(checks)-bad, len(checks))
}
