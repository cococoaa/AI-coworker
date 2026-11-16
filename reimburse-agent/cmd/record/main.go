// record 把真实模型的抽取结果录制下来，供无密钥时回放演示。
//
//	go run ./cmd/record -case data/cases/C01 -out data/replay
//	go run ./cmd/record -all -out data/replay -limit 12
//
// 需要环境变量 DEEPSEEK_API_KEY / ANTHROPIC_API_KEY。缺哪个就只录哪个。
// 录制文件按「供应商/用例编号.json」存放，带 Prompt 版本号；Prompt 一改旧录制自动失效。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"reimburse/internal/llm"
	"reimburse/internal/schema"
)

func main() {
	caseID := flag.String("case", "", "只录这一条，例如 C01")
	all := flag.Bool("all", false, "录制全部用例")
	limit := flag.Int("limit", 0, "配合 -all：最多录多少条（0 表示不限）")
	dataPath := flag.String("data", "data", "测试集根目录")
	out := flag.String("out", "data/replay", "录制输出目录")
	cfgPath := flag.String("config", "", "模型配置文件")
	flag.Parse()

	cfg, err := llm.LoadConfig(*cfgPath)
	if err != nil {
		fatal("读取配置: %v", err)
	}
	if cfg.DeepSeek.APIKey() == "" && cfg.Claude.APIKey() == "" {
		fatal("两个密钥都没配置，没有可录制的内容")
	}

	var cases []*schema.Case
	switch {
	case *caseID != "":
		c, err := schema.LoadCase(filepath.Join(*dataPath, "cases", *caseID))
		if err != nil {
			fatal("读取用例: %v", err)
		}
		cases = []*schema.Case{c}
	case *all:
		cs, err := schema.LoadAll(*dataPath)
		if err != nil {
			fatal("读取测试集: %v", err)
		}
		cases = cs
	default:
		fatal("需要 -case 或 -all")
	}
	if *limit > 0 && len(cases) > *limit {
		cases = cases[:*limit]
	}

	client := llm.NewClient(&cfg)
	rec := &llm.Recorder{Dir: *out}
	fmt.Printf("模型：DeepSeek %s，Claude %s\n", yesNo(cfg.DeepSeek.APIKey() != ""), yesNo(cfg.Claude.APIKey() != ""))
	ok, fail := 0, 0
	for _, c := range cases {
		src := llm.Source{Category: c.Claim.Category, InvoiceText: c.InvoiceText, SupportText: c.SupportText}
		for _, p := range []string{llm.ProviderDeepSeek, llm.ProviderClaude} {
			ep, _ := cfg.Endpoint(p)
			if ep.APIKey() == "" {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			system, user, sch := llm.ExtractPrompt(src)
			resp := client.Call(ctx, p, &llm.Request{
				System: system, Messages: []llm.Message{{Role: "user", Content: user}},
				Schema: sch, SchemaName: "invoice_extraction",
			})
			cancel()
			if resp.Err != nil {
				fmt.Printf("  %s %s 失败：%v\n", c.ID, p, resp.Err)
				fail++
				continue
			}
			if err := rec.Save(p, c.ID, resp.Resp); err != nil {
				fmt.Printf("  %s %s 写入失败：%v\n", c.ID, p, err)
				fail++
				continue
			}
			fmt.Printf("  %s %s 已录制（%d / %d token，%dms）\n", c.ID, p,
				resp.Resp.Usage.InputTokens, resp.Resp.Usage.OutputTokens, resp.Resp.LatencyMS)
			ok++
		}
	}
	fmt.Printf("完成：成功 %d，失败 %d，输出目录 %s\n", ok, fail, *out)
	if ok == 0 {
		os.Exit(1)
	}
}

func yesNo(b bool) string {
	if b {
		return "已配置"
	}
	return "未配置"
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(2)
}
