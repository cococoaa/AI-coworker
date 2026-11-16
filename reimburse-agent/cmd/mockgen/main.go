// mockgen 生成带标准答案的报销测试集。
//
//	go run ./cmd/mockgen                 写入 data/cases/<编号>/
//	go run ./cmd/mockgen -seed 7 -out x  换种子、换目录
//	go run ./cmd/mockgen -dry            只自检与统计，不落盘
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"reimburse/internal/mockgen"
	"reimburse/internal/schema"
	"reimburse/rules"
)

func main() {
	seed := flag.Int64("seed", 20261121, "随机种子，同一种子得到同一批用例")
	out := flag.String("out", "data", "输出根目录，用例写入 <out>/cases/<编号>/")
	rulesPath := flag.String("rules", "", "外部规则表路径，缺省使用内嵌规则表")
	dry := flag.Bool("dry", false, "只自检与统计，不落盘")
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

	cases, err := mockgen.NewGen(*seed, rf).Cases()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		fatal("未写入任何文件")
	}

	// 覆盖检查：规则表里的每个判定项都应至少被一条用例命中。
	covered := map[string]bool{}
	for _, id := range mockgen.Coverage(cases) {
		covered[id] = true
	}
	var missing []string
	for _, id := range rf.AllCheckIDs() {
		if !covered[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		fatal("以下判定项没有用例覆盖: %v", missing)
	}

	if !*dry {
		for _, c := range cases {
			if err := schema.WriteCase(*out, c); err != nil {
				fatal("写入 %s: %v", c.ID, err)
			}
		}
	}
	report(rf, cases, *dry, filepath.Join(*out, "cases"))
}

func report(rf *rules.File, cases []*schema.Case, dry bool, dir string) {
	dec := map[string]int{}
	cat := map[string]int{}
	for _, c := range cases {
		dec[c.Truth.Decision]++
		cat[c.Claim.Category]++
	}
	fmt.Printf("用例 %d 条，判定项 %d/%d 全部覆盖\n", len(cases), len(rf.AllCheckIDs()), len(rf.AllCheckIDs()))
	fmt.Println("按结论:", sortedCounts(dec))
	fmt.Println("按类别:", sortedCounts(cat))
	if dry {
		fmt.Println("dry 模式，未落盘")
	} else {
		fmt.Println("已写入", dir)
	}
}

func sortedCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s := ""
	for _, k := range keys {
		s += fmt.Sprintf(" %s=%d", k, m[k])
	}
	return s
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
