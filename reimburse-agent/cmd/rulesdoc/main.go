// rulesdoc 把规则表（rules.json）生成 Markdown 文档，文档与规则引擎、mock 生成器读同一份数据。
//
//	go run ./cmd/rulesdoc                      写 docs/rules.md（内嵌规则表）
//	go run ./cmd/rulesdoc -cases data          同时列出每个判定项对应的测试用例
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"reimburse/internal/schema"
	"reimburse/rules"
)

var outcomeName = map[string]string{
	rules.Reject:       "驳回",
	rules.ManualReview: "转人工",
	rules.NeedMoreInfo: "退回补材料",
	rules.ApproveAdj:   "调整后通过",
	rules.Approve:      "通过",
}

var catName = map[string]string{"transport": "交通", "lodging": "住宿", "meal": "餐饮招待"}

func main() {
	out := flag.String("out", "docs/rules.md", "输出文件")
	casesDir := flag.String("cases", "", "测试集根目录（含 cases/），给出则追加用例对照列")
	rulesPath := flag.String("rules", "", "外部规则表路径，缺省使用内嵌规则表")
	flag.Parse()

	var rf *rules.File
	var err error
	if *rulesPath != "" {
		rf, err = rules.LoadFile(*rulesPath)
	} else {
		rf, err = rules.Load()
	}
	if err != nil {
		fatal("读取规则表: %v", err)
	}
	byCheck := map[string][]string{}
	if *casesDir != "" {
		cases, err := schema.LoadAll(*casesDir)
		if err != nil {
			fatal("读取测试集: %v", err)
		}
		for _, c := range cases {
			for _, h := range c.Truth.Hits {
				byCheck[h] = append(byCheck[h], c.ID)
			}
		}
	}
	if err := os.WriteFile(*out, []byte(render(rf, byCheck, *casesDir != "")), 0o644); err != nil {
		fatal("写文件: %v", err)
	}
	n := 0
	for _, r := range rf.Rules {
		n += len(r.Checks)
	}
	fmt.Printf("已写入 %s：%d 条规则，%d 个判定项\n", *out, len(rf.Rules), n)
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

func cell(s string) string {
	return strings.NewReplacer("|", "\\|", "\n", " ").Replace(s)
}

func names(m map[string]string, keys []string) string {
	var out []string
	for _, k := range keys {
		if v, ok := m[k]; ok {
			k = v
		}
		out = append(out, k)
	}
	return strings.Join(out, "、")
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
