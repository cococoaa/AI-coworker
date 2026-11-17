// rehearse 是现场前的自动彩排。
//
//	go run ./cmd/rehearse                    无密钥环境：用合成抽取
//	go run ./cmd/rehearse -real              配了密钥：走真实模型
//	go run ./cmd/rehearse -out docs          把记录写到 docs/rehearsal.{json,md}
//
// 退出码：0 全部通过；1 有场景未通过。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"reimburse/internal/llm"
	"reimburse/internal/rehearsal"
	"reimburse/plugins/reimburse-audit/policy"
)

func main() {
	data := flag.String("data", "data", "测试集根目录")
	out := flag.String("out", "", "输出目录；为空只在终端打印")
	real := flag.Bool("real", false, "走真实模型（需要密钥）")
	cfgPath := flag.String("config", "", "模型配置文件")
	rulesPath := flag.String("rules", "", "外部规则表")
	flag.Parse()

	rf, err := loadRules(*rulesPath)
	if err != nil {
		fatal("读取规则表: %v", err)
	}
	cfg, err := llm.LoadConfig(*cfgPath)
	if err != nil {
		fatal("读取模型配置: %v", err)
	}
	useSynth := !*real
	if *real && cfg.DeepSeek.APIKey() == "" && cfg.Claude.APIKey() == "" {
		fatal("-real 需要至少配置一个密钥；无密钥请去掉该参数使用合成抽取")
	}
	if useSynth {
		fmt.Println("模式：合成抽取（无密钥）。三次彩排验证的是链路与降级，不是模型能力。")
	} else {
		fmt.Printf("模式：真实模型。DeepSeek %v，Claude %v\n",
			cfg.DeepSeek.APIKey() != "", cfg.Claude.APIKey() != "")
	}

	r := &rehearsal.Runner{Root: *data, RF: rf, Cfg: cfg, Synth: useSynth}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rep := r.Run(ctx)

	fmt.Println()
	fmt.Printf("彩排 %d/%d 通过\n\n", rep.Passed, rep.Total)
	for _, c := range rep.Cases {
		mark := "✓"
		if !c.Passed {
			mark = "✗"
		}
		fmt.Printf("%s %-28s %s 结论=%-20s 事件=%2d %4dms\n", mark, c.Name, c.CaseID, c.Got, c.Events, c.ElapsedMS)
		if !c.Passed {
			fmt.Printf("    %s\n", c.FailWhy)
		}
	}

	if *out != "" {
		jp, mp, err := rep.Write(*out)
		if err != nil {
			fatal("写彩排记录: %v", err)
		}
		fmt.Printf("\n记录已写入 %s 与 %s\n", jp, mp)
	}
	if !rep.AllOK {
		os.Exit(1)
	}
}

func loadRules(p string) (*policy.File, error) {
	if p == "" {
		return policy.Load()
	}
	return policy.LoadFile(p)
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(2)
}
