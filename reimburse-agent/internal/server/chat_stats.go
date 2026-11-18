package server

import (
	"fmt"
	"strings"
	"time"

	"reimburse/internal/llm"
	"reimburse/internal/task"
	"reimburse/plugins/reimburse-audit/policy"
)

func mentionsAny(text string, words ...string) bool {
	for _, word := range words {
		if strings.Contains(text, word) {
			return true
		}
	}
	return false
}

func asksTodayAuditTotals(text string) bool {
	return mentionsAny(text, "今天", "今日", "当天", "今儿") &&
		mentionsAny(text, "审", "报销", "单据", "转人工") &&
		mentionsAny(text, "多少", "几张", "几单", "几笔", "几轮", "几次", "几个", "几份", "统计", "单量", "多少张")
}

// auditTotals counts completed audit batches and deduplicates cases, keeping the latest outcome from the requested day.
func auditTotals(tasks []*task.Task, now time.Time) (cases, rounds, manual int) {
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	end := start.AddDate(0, 0, 1)
	latest := map[string]task.Run{}
	for _, t := range tasks {
		key := t.CaseID
		if key == "" {
			key = t.ID
		}
		for _, run := range t.Runs {
			if run.Decision == "" || run.At.Before(start) || !run.At.Before(end) {
				continue
			}
			rounds++
			if previous, found := latest[key]; !found || !run.At.Before(previous.At) {
				latest[key] = run
			}
		}
	}
	for _, run := range latest {
		if run.Decision == policy.ManualReview {
			manual++
		}
	}
	return len(latest), rounds, manual
}

func (s *Server) todayAuditReply(messages []llm.Message, now time.Time) (string, bool) {
	last := messages[len(messages)-1].Content
	direct := asksTodayAuditTotals(last)
	followUp := !direct && mentionsAny(last, "人工", "复审", "复核", "几轮", "多少轮", "几次", "多少次") &&
		!mentionsAny(last, "昨天", "前天", "明天", "本周", "上周", "本月", "上月")
	if !direct {
		if !followUp || len(messages) < 3 || !asksTodayAuditTotals(messages[len(messages)-3].Content) {
			return "", false
		}
	}
	if s.Tasks == nil {
		return "当前服务没有启用审核记录库，暂时查不到今天的单量。", true
	}
	cases, rounds, manual := auditTotals(s.Tasks.List(), now)
	var reply string
	switch {
	case followUp && mentionsAny(last, "人工", "复审", "复核"):
		reply = fmt.Sprintf("今天已审的单据中，有 %d 张在最后一轮转人工复审。", manual)
	case cases == 0:
		reply = "今天还没有已完成的初审。"
	default:
		reply = fmt.Sprintf("今天初审了 %d 张单，共 %d 轮。按每张单今天的最后一轮结果，有 %d 张转人工复审。", cases, rounds, manual)
	}
	if mentionsAny(last, "累吗", "累么", "累不累", "辛苦") {
		reply += "我不累，有新单可以继续发给我。"
	}
	return reply, true
}
