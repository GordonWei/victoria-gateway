package aiops

import (
	"fmt"
	"strings"

	"github.com/gordonwei/orch-gateway/pkg/model"
)

// maxLogLinesInPrompt caps how many log lines get sent to the LLM. Local
// models have small context windows and this is a summarizer, not a log
// viewer — if there are more lines than this, keep the most recent ones
// (closest to when the alert fired) and note how many were dropped.
const maxLogLinesInPrompt = 80

const systemPrompt = `你是一個地端基礎設施的 AIOps 助理，負責把一則告警跟它相關的 log 整理成給值班工程師看的簡短摘要。

規則：
- 用繁體中文回答
- 先用一句話講清楚「大概是什麼問題」
- 再列 2-4 個從 log 裡看到的具體線索（不要重複貼整段 log，要消化過再講重點）
- 如果 log 內容看起來跟告警本身無關或不足以判斷根因，誠實講「log 內容不足以判斷」，不要編造原因
- 最後給一個建議的下一步（要查什麼、要做什麼），不要下沒有根據的肯定結論`

// Summarizer turns an alert + its surrounding logs into a plain-language
// incident summary via a local LLM (LM Studio, MLX, Ollama — anything
// pkg/model.OpenAIClient already speaks).
type Summarizer struct {
	llm model.LLM
}

func NewSummarizer(llm model.LLM) *Summarizer {
	return &Summarizer{llm: llm}
}

// Summarize produces a plain-language incident summary for one alert.
// logs should already be scoped to the alert's host and time window (see
// loki.go's QueryRange) — this function does not re-filter them.
func (s *Summarizer) Summarize(alert Alert, logs []LogEntry) (string, error) {
	if s.llm == nil {
		return "", fmt.Errorf("summarize: no LLM configured")
	}

	prompt, err := buildPrompt(alert, logs)
	if err != nil {
		return "", fmt.Errorf("summarize: build prompt: %w", err)
	}

	messages := []model.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: prompt},
	}

	// MaxTokens raised from an initial 512: google/gemma-4-26b-a4b-qat is a
	// reasoning model that spends tokens on hidden reasoning_content before
	// writing visible content, and 512 was getting fully consumed by
	// reasoning on real (longer, Chinese-language) prompts, leaving an
	// empty visible reply. Confirmed via a real end-to-end test against the
	// live LM Studio instance on 2026-08-22.
	reply, err := s.llm.Chat(messages, &model.ChatOptions{MaxTokens: 2048, Temperature: 0.2})
	if err != nil {
		return "", fmt.Errorf("summarize: %s chat failed: %w", s.llm.Backend(), err)
	}
	if strings.TrimSpace(reply) == "" {
		return "", fmt.Errorf("summarize: %s returned an empty reply", s.llm.Backend())
	}
	return reply, nil
}

func buildPrompt(alert Alert, logs []LogEntry) (string, error) {
	host, ok := alert.Host()
	if !ok {
		return "", fmt.Errorf("alert has neither \"host\" nor \"instance\" label, fingerprint=%s", alert.Fingerprint)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "告警名稱：%s\n", alert.Labels["alertname"])
	fmt.Fprintf(&b, "主機：%s\n", host)
	fmt.Fprintf(&b, "狀態：%s\n", alert.Status)
	if summary, ok := alert.Annotations["summary"]; ok && summary != "" {
		fmt.Fprintf(&b, "告警描述：%s\n", summary)
	}
	if desc, ok := alert.Annotations["description"]; ok && desc != "" {
		fmt.Fprintf(&b, "詳細說明：%s\n", desc)
	}

	b.WriteString("\n相關 log：\n")
	if len(logs) == 0 {
		b.WriteString("（這個時間窗內沒有查到任何 log）\n")
	} else {
		start := 0
		dropped := 0
		if len(logs) > maxLogLinesInPrompt {
			dropped = len(logs) - maxLogLinesInPrompt
			start = dropped
		}
		if dropped > 0 {
			fmt.Fprintf(&b, "（log 總共 %d 行，只顯示最近的 %d 行，省略較早的 %d 行）\n", len(logs), maxLogLinesInPrompt, dropped)
		}
		for _, entry := range logs[start:] {
			fmt.Fprintf(&b, "[%s] %s\n", entry.Timestamp.Format("15:04:05"), entry.Line)
		}
	}

	return b.String(), nil
}
