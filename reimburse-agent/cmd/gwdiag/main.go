package main

// 逐步缩小范围的网关诊断：用项目真实的 Prompt 与请求体，一次只改一个变量，
// 打印每次的 HTTP 状态，用于定位"哪一项导致 401/403"。
// 令牌与网关地址从环境变量读取，不打印令牌。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"reimburse/internal/llm"
	"reimburse/internal/schema"
)

func main() {
	base := os.Getenv("GW_BASE")
	if base == "" {
		base = "https://api.openai-next.com"
	}
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		fmt.Println("缺少 ANTHROPIC_API_KEY")
		os.Exit(2)
	}
	c, err := schema.LoadCase("data/cases/C01")
	if err != nil {
		fmt.Println(err)
		os.Exit(2)
	}
	system, user, sch := llm.ExtractPrompt(llm.Source{Category: c.Claim.Category,
		InvoiceText: c.InvoiceText, SupportText: c.SupportText})

	type variant struct {
		name    string
		headers map[string]string
		body    map[string]any
	}
	full := map[string]any{
		"model": "claude-sonnet-5-5", "system": system,
		"messages":   []map[string]any{{"role": "user", "content": user}},
		"max_tokens": 4096, "temperature": 0, "stream": false,
		"output_config": map[string]any{"format": map[string]any{"type": "json_schema", "schema": sch}},
	}
	noOutput := map[string]any{}
	for k, v := range full {
		noOutput[k] = v
	}
	delete(noOutput, "output_config")
	shortPrompt := map[string]any{}
	for k, v := range full {
		shortPrompt[k] = v
	}
	shortPrompt["system"] = "你是票据录入员，只输出 json。"
	shortPrompt["messages"] = []map[string]any{{"role": "user", "content": "发票号码：12345\n输出 json"}}

	variants := []variant{
		{"完整请求", map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01", "Accept": "application/json"}, full},
		{"完整请求+UA", map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01", "Accept": "application/json",
			"User-Agent": ua()}, full},
		{"去掉 output_config", map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01", "Accept": "application/json"}, noOutput},
		{"短 Prompt", map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01", "Accept": "application/json"}, shortPrompt},
		{"加 Bearer", map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01", "Accept": "application/json",
			"Authorization": "Bearer " + key}, full},
	}

	client := &http.Client{Timeout: 120 * time.Second}
	for _, v := range variants {
		b, _ := json.Marshal(v.body)
		req, _ := http.NewRequest("POST", base+"/v1/messages", bytes.NewReader(b))
		for k, val := range v.headers {
			req.Header.Set(k, val)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("%-16s 请求失败：%v\n", v.name, err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		resp.Body.Close()
		fmt.Printf("%-16s %d  body=%d字节  首段=%s\n", v.name, resp.StatusCode, len(b), snippet(body))
	}
}

func ua() string {
	return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
}

func snippet(b []byte) string {
	s := string(b)
	if len([]rune(s)) > 160 {
		return string([]rune(s)[:160])
	}
	return s
}
