package server

import (
	"fmt"
	"strings"

	"reimburse/internal/schema"
	"reimburse/internal/task"
)

// 把补交内容真正写回单据。
//
// 这是"闭环"最容易被做成假的一步。补交清单里列了「缺申请单号」，
// 报销人补了单号，登记成功、状态也变成"待审核"——看起来闭环了。
// 但如果续审时仍然拿原始报销单去跑，R14-b 还是会命中，结论一点没变。
// 用户会觉得"我明明补了，你怎么还说缺"。
//
// 所以这里按补件类型把内容落到对应字段上，续审用的就是补交后的单据。
//
// 约定：
//   - support        整份佐证凭证，替换 SupportText
//   - claim_info     报销单信息，正文为 key=value，可一次补多项
//   - 其它（字段名） 直接对应报销单里的同名字段
//
// 认不出的键不会被悄悄丢掉：applyToClaim 会返回未识别的键，由调用方提示。

// applySupplements 把所有已补交的材料写进单据副本，返回未识别的键。
func applySupplements(c *schema.Case, t *task.Task) (*schema.Case, []string) {
	eff := *c
	eff.Claim = c.Claim
	var unknown []string

	for _, a := range t.Attachments {
		if strings.TrimSpace(a.Content) == "" {
			continue
		}
		// 同一项可能补了多版，只取最新一版（附件按时间追加，后者覆盖前者）。
		if a.Kind == "support" {
			eff.SupportText = a.Content
			eff.Claim.Attachments.SupportFile = "support.txt"
			continue
		}
		switch a.Kind {
		case "late_explanation":
			eff.Claim.LateExplanation = strings.TrimSpace(a.Content)
			continue
		case "finance_request":
			// 自由文本留在任务附件中，供财务复核。
			continue
		}
		// claim_info 与字段名两类都走键值映射。
		body := a.Content
		if a.Kind == "claim_info" {
			// 报销单信息可能一次补多项，正文按 key=value 逐行给。
			unknown = append(unknown, applyKeyValues(&eff.Claim, body)...)
			continue
		}
		// 单项补交：kind 就是字段名，正文本身就是值。
		if !setClaimField(&eff.Claim, a.Kind, body) {
			unknown = append(unknown, a.Kind)
		}
	}
	return &eff, unknown
}

// applyKeyValues 解析 "request_no=SQ001" / "purpose=现场实施" 这类正文。
// 也接受「申请单号：SQ001」这种中文写法，因为报销人不会照抄英文键名。
func applyKeyValues(cl *schema.Claim, body string) []string {
	var unknown []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := splitPair(line)
		if !ok {
			continue
		}
		if !setClaimField(cl, k, v) {
			unknown = append(unknown, k)
		}
	}
	return unknown
}

// splitPair 拆出键值对，支持 = ： : 三种分隔。
func splitPair(line string) (string, string, bool) {
	for _, sep := range []string{"=", "：", ":"} {
		if i := strings.Index(line, sep); i > 0 {
			return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+len(sep):]), true
		}
	}
	return "", "", false
}

// setClaimField 把值写进报销单的对应字段。返回 false 表示这个键不认识。
func setClaimField(cl *schema.Claim, key, val string) bool {
	val = strings.TrimSpace(val)
	if val == "" {
		return true // 空值当作"没补"，不算错
	}
	switch normalizeKey(key) {
	case "request_no", "申请单号", "申请单编号", "出差申请单号":
		cl.RequestNo = val
	case "purpose", "事由", "出差事由", "费用事由":
		cl.Purpose = val
	case "internal_count", "内部人数", "我方人数":
		if n, err := parseInt(val); err == nil {
			cl.InternalCount = n
		}
	case "external_count", "外部人数", "招待人数":
		if n, err := parseInt(val); err == nil {
			cl.ExternalCount = n
		}
	case "external_org", "招待对象单位", "接待单位":
		cl.ExternalOrg = val
	case "entertain_approval_no", "事前审批号", "招待审批号":
		cl.EntertainApprovalNo = val
	case "special_approval_no", "特批单号", "特殊审批号":
		cl.SpecialApprovalNo = val
	default:
		return false
	}
	return true
}

// normalizeKey 统一大小写与常见写法差异，让"申请单号"与"request_no"等价。
func normalizeKey(k string) string {
	k = strings.TrimSpace(k)
	k = strings.TrimSuffix(k, "：")
	k = strings.TrimSuffix(k, ":")
	return strings.ToLower(k)
}

// parseInt 只接受十进制正整数，拒绝空与非法输入（不把垃圾当 0）。
func parseInt(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("人数不能为负")
	}
	return n, nil
}
