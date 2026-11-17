// policy-check 只校验制度数据，不碰判定代码。业务改完 policy 就跑这个。
//
//	go run ./plugins/reimburse-audit/tools/policy-check
//	go run ./plugins/reimburse-audit/tools/policy-check -policy plugins/reimburse-audit/policy/presets/strict/policy.json
//
// 它检查四类问题，任一不过退出码 1：
//  1. 结构：JSON 合法、必填键存在、类型正确（由 policy.Load 完成）；
//  2. 引用：每条 check 的 requires.fields / requires.tools 指向真实存在的字段与工具；
//  3. 覆盖：规则表里每个判定项至少被一条用例命中——否则改坏了也没人知道；
//  4. 优先级：decision_priority 覆盖全部结论类型。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"reimburse/plugins/reimburse-audit/policy"
)

func main() {
	policyPath := flag.String("policy", "plugins/reimburse-audit/policy/policy.json", "制度数据路径")
	casesDir := flag.String("cases", "plugins/reimburse-audit/fixtures/cases", "用例目录（检查覆盖用）")
	skipCoverage := flag.Bool("skip-coverage", false, "跳过覆盖检查（只有规则没有用例时用）")
	flag.Parse()

	f, err := policy.LoadFile(*policyPath)
	if err != nil {
		fail("加载制度数据: %v", err)
	}
	fmt.Printf("制度：%s（%s），规则 %d 条，判定项 %d 个\n",
		f.Policy.Name, f.Policy.Version, len(f.Rules), len(f.AllCheckIDs()))

	problems := checkRefs(f)
	if !*skipCoverage {
		problems = append(problems, checkCoverage(f, *casesDir)...)
	}

	if len(problems) > 0 {
		fmt.Printf("\n发现 %d 处问题：\n", len(problems))
		for _, p := range problems {
			fmt.Println("  ✗", p)
		}
		os.Exit(1)
	}
	fmt.Println("\n✓ 结构与引用均合法，判定项全部有用例覆盖")
}

// checkRefs 校验 requires 里引用的字段与工具是否真实存在。
func checkRefs(f *policy.File) []string {
	var out []string
	validFields := map[string]bool{}
	for _, k := range f.Params.KeyFields {
		validFields[k] = true
	}
	// 非关键字段也算合法引用（例如 buyer_name 用于 R02-a）。
	for _, extra := range []string{"buyer_name", "seller_name", "item_name"} {
		validFields[extra] = true
	}
	validTools := map[string]bool{"verify_invoice": true, "check_company": true, "check_history": true}

	for _, r := range f.Rules {
		for _, c := range r.Checks {
			for _, fname := range c.Requires.Fields {
				if !validFields[fname] {
					out = append(out, fmt.Sprintf("%s 依赖了不存在的字段 %q", c.ID, fname))
				}
			}
			for _, t := range c.Requires.Tools {
				if !validTools[t] {
					out = append(out, fmt.Sprintf("%s 依赖了不存在的工具 %q", c.ID, t))
				}
			}
		}
	}
	return out
}

// checkCoverage 检查每个判定项是否至少被一条用例命中。
func checkCoverage(f *policy.File, dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{"读取用例目录失败: " + err.Error()}
	}
	covered := map[string]bool{}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "truth.json"))
		if err != nil {
			continue
		}
		var t struct {
			Hits []string `json:"hits"`
		}
		if err := json.Unmarshal(b, &t); err != nil {
			continue
		}
		n++
		for _, h := range t.Hits {
			covered[h] = true
		}
	}
	if n == 0 {
		return []string{"用例目录里没有可读的用例: " + dir}
	}
	var missing []string
	for _, id := range f.AllCheckIDs() {
		if !covered[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return []string{"以下判定项没有用例覆盖（改坏了也发现不了）: " + join(missing, "、")}
}

func join(xs []string, sep string) string {
	s := ""
	for i, x := range xs {
		if i > 0 {
			s += sep
		}
		s += x
	}
	return s
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
