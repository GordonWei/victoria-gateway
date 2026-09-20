package aiops

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/model"
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
- 最後給一個建議的下一步（要查什麼、要做什麼），不要下沒有根據的肯定結論

輸出格式：
你必須只回覆一個 JSON 物件，不要加任何其他文字、不要用 markdown code fence 包起來，格式如下：
{"summary": "上面規則產出的完整摘要文字", "confidence": "low|medium|high", "escalate": true|false, "reason": "一句話說明為什麼給這個 confidence／要不要 escalate"}

confidence 代表你對這次判斷的把握程度；escalate 代表你認為這個問題複雜到超出你的能力範圍、需要交給更強的模型重新分析（例如：log 內容不足以判斷、牽涉到你不熟悉的服務關聯、你的 confidence 是 low）。診斷得出明確結論、把握度高時，escalate 應該是 false。`

// SummarizeResult is the local model's structured judgment about one
// alert: the human-readable summary plus a self-reported signal about
// whether this needs a stronger (cloud) model's attention. It's a signal,
// not a verdict — see ShouldEscalate, which combines it with the
// operator's own escalation rules rather than trusting it alone (small
// local models are not reliably calibrated about their own confidence).
type SummarizeResult struct {
	Summary    string
	Confidence string // "low" | "medium" | "high" | "unknown" (set on parse failure)
	Escalate   bool
	Reason     string
	// ParseFailed is true if the model didn't return valid structured
	// JSON and Summary/Reason etc. fell back to the raw reply text.
	// Exposed so callers can log it without treating it as a hard error —
	// a malformed-but-non-empty reply is still useful to a human.
	ParseFailed bool
}

// Summarizer turns an alert + its surrounding logs into a plain-language
// incident summary via a local LLM (LM Studio, MLX, Ollama — anything
// pkg/model.OpenAIClient already speaks).
type Summarizer struct {
	llm model.LLM
}

func NewSummarizer(llm model.LLM) *Summarizer {
	return &Summarizer{llm: llm}
}

// Summarize produces a structured incident summary for one alert using
// the Summarizer's configured LLM. logs should already be scoped to the
// alert's host and time window (see loki.go's QueryRange) — this function
// does not re-filter them. ragContext, if non-empty, is prior similar
// incidents retrieved from the RAG store (see pkg/rag) and is inserted
// into the prompt as extra reference material; pass "" when RAG is
// disabled or nothing relevant was found.
func (s *Summarizer) Summarize(alert Alert, logs []LogEntry, ragContext string) (SummarizeResult, error) {
	if s.llm == nil {
		return SummarizeResult{}, fmt.Errorf("summarize: no LLM configured")
	}
	return SummarizeWithLLM(s.llm, alert, logs, ragContext)
}

// SummarizeWithLLM runs the same prompt against an arbitrary LLM backend.
// It's a package-level function rather than a Summarizer method so the
// triage escalation path (cmd/victoria-gateway's handler) can reuse the exact
// same prompt-building and JSON-parsing logic against a cloud model
// without needing a second Summarizer instance wrapping it.
func SummarizeWithLLM(llm model.LLM, alert Alert, logs []LogEntry, ragContext string) (SummarizeResult, error) {
	prompt, err := buildPrompt(alert, logs, ragContext)
	if err != nil {
		return SummarizeResult{}, fmt.Errorf("summarize: build prompt: %w", err)
	}

	messages := []model.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: prompt},
	}

	// MaxTokens raised from an initial 512, then 2048, now 4096:
	// google/gemma-4-26b-a4b-qat is a reasoning model whose OpenAI-
	// compatible reply carries a separate reasoning_content field (hidden
	// "thinking") that draws from the same MaxTokens budget as the
	// visible content — and how much it spends on reasoning is not
	// deterministic even at this low a temperature. Confirmed by direct
	// reproduction on 2026-09-20: the identical prompt sent 5 times
	// measured reasoning_tokens ranging 806–1202, a ~50% swing run to
	// run. 512→2048 (2026-08-22) fixed the failure mode for typical
	// prompts; a GCP Cloud Logging-sourced prompt (verbose, repetitive
	// stack-trace-style lines) reproduced the exact same failure against
	// 2048 in real end-to-end testing the same night — an unlucky
	// sampling draw spent the entire budget on reasoning, leaving zero
	// tokens for content. This isn't a GCP-specific problem: any
	// sufficiently long/complex prompt (Loki or CloudWatch content, too)
	// has the same failure mode available to it, just a lower chance of
	// hitting it. Raising the ceiling again lowers that chance further
	// without changing the underlying non-determinism — see
	// chatWithEmptyReplyRetry below for the other half of the mitigation.
	reply, err := chatWithEmptyReplyRetry(llm, messages)
	if err != nil {
		return SummarizeResult{}, err
	}

	return parseSummarizeReply(reply), nil
}

// summarizeMaxTokens bounds one Chat call's reply, shared by both the
// initial attempt and the empty-reply retry — see SummarizeWithLLM's doc
// comment on the reasoning-model token budget this exists to give enough
// headroom against.
const summarizeMaxTokens = 4096

// emptyReplyRetrySleep is swapped out by tests so the retry path doesn't
// wait a real delay — same pattern as loki.go's retrySleep.
var emptyReplyRetrySleep = time.Sleep

// chatWithEmptyReplyRetry calls llm.Chat and retries once, after a short
// pause, if the reply comes back empty. An empty reply isn't a network or
// server error (Chat returned err == nil) — see SummarizeWithLLM's doc
// comment — so retrying is a bet on a better sampling draw next time, the
// same "one retry, cheap enough" tradeoff loki.go's getWithOneRetry makes
// for actual transient failures. A second empty reply in a row is treated
// as this alert's analysis failing, not retried further: unlike a Loki
// blip, nothing suggests a third attempt is more likely to land than the
// second.
func chatWithEmptyReplyRetry(llm model.LLM, messages []model.Message) (string, error) {
	reply, err := llm.Chat(messages, &model.ChatOptions{MaxTokens: summarizeMaxTokens, Temperature: 0.2})
	if err != nil {
		return "", fmt.Errorf("summarize: %s chat failed: %w", llm.Backend(), err)
	}
	if strings.TrimSpace(reply) != "" {
		return reply, nil
	}

	log.Printf("aiops: %s returned an empty reply (likely hidden reasoning content exhausted the token budget), retrying once", llm.Backend())
	emptyReplyRetrySleep(500 * time.Millisecond)

	reply, err = llm.Chat(messages, &model.ChatOptions{MaxTokens: summarizeMaxTokens, Temperature: 0.2})
	if err != nil {
		return "", fmt.Errorf("summarize: %s chat failed on retry: %w", llm.Backend(), err)
	}
	if strings.TrimSpace(reply) == "" {
		return "", fmt.Errorf("summarize: %s returned an empty reply twice in a row", llm.Backend())
	}
	return reply, nil
}

// parseSummarizeReply decodes the model's JSON reply into a
// SummarizeResult. Models occasionally ignore the "no markdown fence"
// instruction and wrap the JSON in ```json ... ``` anyway, so that's
// stripped before parsing is attempted. If the reply still isn't valid
// JSON, this falls back to treating the whole reply as the summary text
// with escalate=false — a malformed reply about a real problem is still
// worth showing a human, and defaulting escalate to false means a parse
// failure can't itself trigger an unbounded cloud-escalation loop.
func parseSummarizeReply(reply string) SummarizeResult {
	candidate := strings.TrimSpace(reply)
	candidate = strings.TrimPrefix(candidate, "```json")
	candidate = strings.TrimPrefix(candidate, "```")
	candidate = strings.TrimSuffix(candidate, "```")
	candidate = strings.TrimSpace(candidate)

	var parsed struct {
		Summary    string `json:"summary"`
		Confidence string `json:"confidence"`
		Escalate   bool   `json:"escalate"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(candidate), &parsed); err != nil || strings.TrimSpace(parsed.Summary) == "" {
		return SummarizeResult{
			Summary:     reply,
			Confidence:  "unknown",
			Escalate:    false,
			Reason:      "model did not return valid structured JSON; showing raw reply",
			ParseFailed: true,
		}
	}

	return SummarizeResult{
		Summary:    parsed.Summary,
		Confidence: parsed.Confidence,
		Escalate:   parsed.Escalate,
		Reason:     parsed.Reason,
	}
}

// ShouldEscalate decides whether an alert's local-model analysis should be
// re-run against a stronger cloud model. It ORs two independent signals
// together, deliberately not trusting the local model's self-report
// alone: small local models aren't reliably calibrated about their own
// confidence, so an operator-defined allowlist of alert names always
// wins regardless of what the local model thinks.
func ShouldEscalate(alertName string, local SummarizeResult, alwaysCloud []string) (escalate bool, reason string) {
	for _, name := range alwaysCloud {
		if strings.EqualFold(strings.TrimSpace(name), alertName) {
			return true, fmt.Sprintf("alertname %q is in the always_cloud escalation list", alertName)
		}
	}
	if local.Escalate {
		if local.Reason != "" {
			return true, "local model requested escalation: " + local.Reason
		}
		return true, "local model requested escalation"
	}
	return false, ""
}

func buildPrompt(alert Alert, logs []LogEntry, ragContext string) (string, error) {
	display, _, ok := alert.AffectedIdentity()
	if !ok {
		return "", fmt.Errorf("alert has neither \"host\"/\"instance\" nor \"namespace\"+\"pod\" labels, fingerprint=%s", alert.Fingerprint)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "告警名稱：%s\n", alert.Labels["alertname"])
	fmt.Fprintf(&b, "主機：%s\n", display)
	fmt.Fprintf(&b, "狀態：%s\n", alert.Status)
	if summary, ok := alert.Annotations["summary"]; ok && summary != "" {
		fmt.Fprintf(&b, "告警描述：%s\n", summary)
	}
	if desc, ok := alert.Annotations["description"]; ok && desc != "" {
		fmt.Fprintf(&b, "詳細說明：%s\n", desc)
	}

	if strings.TrimSpace(ragContext) != "" {
		b.WriteString("\n過去類似事件（供參考，不代表這次一定是同樣原因）：\n")
		b.WriteString(ragContext)
		b.WriteString("\n")
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
