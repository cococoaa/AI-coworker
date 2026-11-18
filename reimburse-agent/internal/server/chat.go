package server

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"reimburse/internal/llm"
)

const coworkerPrompt = "你是报销同事。用中文自然交流，可以闲聊，通常用一到三句话。遇到某张单据的审核、复审、补件或查进度，提醒用户用页面对应按钮；你只处理当前对话，没有查阅单据或执行操作的权限，不要声称已经审核或查询。不要把闲聊里的描述作为审核依据，不要索取税号、发票号码等敏感资料。"

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != "application/json" {
		jsonOut(w, map[string]any{"error": "请发送 JSON 对话内容"}, http.StatusUnsupportedMediaType)
		return
	}
	var req struct {
		Messages []llm.Message `json:"messages"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 96<<10)).Decode(&req); err != nil || len(req.Messages) == 0 || len(req.Messages) > 9 || len(req.Messages)%2 == 0 {
		jsonOut(w, map[string]any{"error": "对话内容无效，请重新发送"}, http.StatusBadRequest)
		return
	}
	for i, m := range req.Messages {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if m.Role != role || strings.TrimSpace(m.Content) == "" || utf8.RuneCountInString(m.Content) > 1600 {
			jsonOut(w, map[string]any{"error": "请将每条消息控制在 1600 字以内"}, http.StatusBadRequest)
			return
		}
	}

	if reply, ok := s.todayAuditReply(req.Messages, time.Now()); ok {
		jsonOut(w, map[string]string{"reply": reply, "mode": "business"}, http.StatusOK)
		return
	}

	client := quotaClient(r)
	if s.Quota != nil && s.Quota.Check(client).Blocked {
		jsonOut(w, map[string]string{"reply": "这周的模型额度已用完，暂时不能继续闲聊。", "mode": "local"}, http.StatusOK)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	if !s.DemoSynthetic {
		for _, provider := range []string{"claude", "deepseek"} {
			ep, ok := s.Cfg.Endpoint(provider)
			if !ok || ep.APIKey() == "" || ctx.Err() != nil {
				continue
			}
			call := s.LLM().Call(ctx, provider, &llm.Request{
				System: coworkerPrompt, Messages: req.Messages, MaxTokens: 300, Temperature: 0.65,
			})
			if call.Err != nil || call.Resp == nil {
				continue
			}
			if s.Quota != nil {
				usage := call.Resp.Usage
				s.Quota.Record(client, ep.CostUSD(usage.InputTokens, usage.OutputTokens), "对话")
			}
			jsonOut(w, map[string]string{"reply": strings.TrimSpace(call.Resp.Text), "mode": "model"}, http.StatusOK)
			return
		}
	}
	last := strings.TrimSpace(req.Messages[len(req.Messages)-1].Content)
	jsonOut(w, map[string]string{"reply": offlineChatReply(last), "mode": "local"}, http.StatusOK)
}

func offlineChatReply(message string) string {
	switch strings.ToLower(strings.Trim(message, "！!。？? ")) {
	case "你好", "您好", "hi", "hello", "嗨":
		return "你好，我是你的报销同事。今天有什么事要一起看看？"
	case "谢谢", "多谢", "辛苦了":
		return "不客气，有需要随时叫我。"
	default:
		if strings.Contains(message, "笑话") || strings.Contains(message, "段子") {
			return "讲个短的：猫问我周末有什么安排。我说睡觉。它点点头，说「周一到周五呢？」"
		}
		if strings.Contains(message, "发票") || strings.Contains(message, "报销") {
			return "可以点「发票送审」提交材料，有结果后我会在这里回复。"
		}
		return "闲聊暂时连不上模型。可以稍后再试，或先发送发票。"
	}
}
