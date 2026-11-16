package llm

import (
	"encoding/json"
	"testing"
)

// 网关返回的 usage 在真实响应里是完整对象，但不同网关字段不统一；
// 这里锁死三种形态都能解析出输入/输出 token。
func TestDecodeUsageVariants(t *testing.T) {
	cases := []struct {
		name             string
		raw              string
		in, out, cacheIn int
	}{
		{"anthropic", `{"input_tokens":39,"output_tokens":40,"cache_read_input_tokens":7}`, 39, 40, 7},
		{"openai", `{"prompt_tokens":1180,"completion_tokens":240,"prompt_cache_hit_tokens":900}`, 1180, 240, 900},
		{"带额外字段", `{"input_tokens":56,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":99,"service_tier":"standard","output_tokens_details":{"thinking_tokens":81}}`, 56, 99, 0},
		{"缺失", `null`, 0, 0, 0},
	}
	for _, c := range cases {
		var in, out, hit int
		decodeUsage(json.RawMessage(c.raw), &in, &out, &hit)
		if in != c.in || out != c.out || hit != c.cacheIn {
			t.Errorf("%s: 得到 in=%d out=%d hit=%d，期望 %d/%d/%d", c.name, in, out, hit, c.in, c.out, c.cacheIn)
		}
	}
}
