package aiops

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/model"
)

func TestParseSummarizeReply_ValidJSON(t *testing.T) {
	reply := `{"summary": "CPU 過載", "confidence": "high", "escalate": false, "reason": "log 很清楚"}`
	got := parseSummarizeReply(reply)
	if got.ParseFailed {
		t.Error("expected ParseFailed=false for valid JSON")
	}
	if got.Summary != "CPU 過載" || got.Confidence != "high" || got.Escalate || got.Reason != "log 很清楚" {
		t.Errorf("unexpected result: %+v", got)
	}
}

func TestParseSummarizeReply_MarkdownFence(t *testing.T) {
	reply := "```json\n{\"summary\": \"CPU 過載\", \"confidence\": \"high\", \"escalate\": false, \"reason\": \"ok\"}\n```"
	got := parseSummarizeReply(reply)
	if got.ParseFailed {
		t.Error("expected the ```json fence to be stripped before parsing")
	}
	if got.Summary != "CPU 過載" {
		t.Errorf("summary = %q", got.Summary)
	}
}

func TestParseSummarizeReply_MalformedFallsBackToRawText(t *testing.T) {
	reply := "CPU 持續偏高，log 顯示已達 97%，建議檢查該主機負載來源。"
	got := parseSummarizeReply(reply)
	if !got.ParseFailed {
		t.Error("expected ParseFailed=true for a plain-text (non-JSON) reply")
	}
	if got.Summary != reply {
		t.Errorf("expected the raw reply to be used as Summary, got %q", got.Summary)
	}
	if got.Escalate {
		t.Error("a parse failure must not itself trigger escalation")
	}
	if got.Confidence != "unknown" {
		t.Errorf("confidence = %q, want %q", got.Confidence, "unknown")
	}
}

func TestParseSummarizeReply_EmptySummaryField(t *testing.T) {
	// Valid JSON, but an empty summary field is treated the same as
	// malformed — there's nothing useful to show a human either way.
	reply := `{"summary": "", "confidence": "low", "escalate": true, "reason": "x"}`
	got := parseSummarizeReply(reply)
	if !got.ParseFailed {
		t.Error("expected ParseFailed=true for an empty summary field")
	}
}

func TestShouldEscalate_AlwaysCloudList(t *testing.T) {
	local := SummarizeResult{Escalate: false, Confidence: "high"}
	escalate, reason := ShouldEscalate("InstanceDown", local, []string{"instancedown"})
	if !escalate {
		t.Error("expected escalation: alertname is in the always_cloud list (case-insensitive)")
	}
	if !strings.Contains(reason, "always_cloud") {
		t.Errorf("reason = %q, expected it to mention always_cloud", reason)
	}
}

func TestShouldEscalate_LocalModelRequests(t *testing.T) {
	local := SummarizeResult{Escalate: true, Reason: "log 內容不足以判斷"}
	escalate, reason := ShouldEscalate("SomeOtherAlert", local, nil)
	if !escalate {
		t.Error("expected escalation: local model set escalate=true")
	}
	if !strings.Contains(reason, "log 內容不足以判斷") {
		t.Errorf("reason = %q, expected it to include the local model's own reason", reason)
	}
}

func TestShouldEscalate_NoEscalation(t *testing.T) {
	local := SummarizeResult{Escalate: false, Confidence: "high"}
	escalate, _ := ShouldEscalate("SomeAlert", local, []string{"OtherAlert"})
	if escalate {
		t.Error("expected no escalation when neither signal fires")
	}
}

func TestBuildPrompt_IncludesRAGContext(t *testing.T) {
	alert := Alert{
		Labels: map[string]string{"alertname": "InstanceDown", "host": "172.16.100.7"},
		Status: "firing",
	}
	prompt, err := buildPrompt(alert, nil, "1. [2026-07-26] InstanceDown（主機：172.16.100.7）\n   後來確認：舊測試機殘留 target，已下線\n")
	if err != nil {
		t.Fatalf("buildPrompt: %v", err)
	}
	if !strings.Contains(prompt, "過去類似事件") || !strings.Contains(prompt, "舊測試機殘留 target") {
		t.Errorf("prompt missing RAG context: %q", prompt)
	}
}

func TestBuildPrompt_NoRAGContext(t *testing.T) {
	alert := Alert{
		Labels: map[string]string{"alertname": "InstanceDown", "host": "172.16.100.7"},
		Status: "firing",
	}
	prompt, err := buildPrompt(alert, nil, "")
	if err != nil {
		t.Fatalf("buildPrompt: %v", err)
	}
	if strings.Contains(prompt, "過去類似事件") {
		t.Error("expected no RAG section when ragContext is empty")
	}
}

// TestSummarizeWithLLM_StructuredReply exercises the whole Summarize path
// (not just the parser in isolation) against a fake LLM that returns the
// structured JSON shape the system prompt asks for.
func TestSummarizeWithLLM_StructuredReply(t *testing.T) {
	fake := &fakeLLM{reply: `{"summary": "CPU 過載", "confidence": "high", "escalate": false, "reason": "log 很清楚"}`}
	alert := Alert{Labels: map[string]string{"alertname": "cpu_high", "host": "h"}, Status: "firing"}

	result, err := SummarizeWithLLM(fake, alert, nil, "")
	if err != nil {
		t.Fatalf("SummarizeWithLLM: %v", err)
	}
	if result.Summary != "CPU 過載" || result.Escalate {
		t.Errorf("unexpected result: %+v", result)
	}
}

// fakeLLM is a minimal model.LLM for unit tests that don't need real
// HTTP — httptest is used where the request shape itself matters
// (see main_test.go / model_test.go), this is for pure logic tests.
type fakeLLM struct {
	reply string
	err   error
}

func (f *fakeLLM) Chat(messages []model.Message, opts *model.ChatOptions) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.reply, nil
}
func (f *fakeLLM) Available() bool   { return true }
func (f *fakeLLM) ModelName() string { return "fake" }
func (f *fakeLLM) Backend() string   { return "fake" }

// sequencedLLM returns a different reply on each successive Chat call,
// for testing chatWithEmptyReplyRetry's retry-once behavior — fakeLLM
// above always returns the same reply, which can't exercise "empty then
// non-empty" or "empty twice."
type sequencedLLM struct {
	replies []string // consumed in order; Chat panics if called more times than len(replies)
	calls   int
}

func (f *sequencedLLM) Chat(messages []model.Message, opts *model.ChatOptions) (string, error) {
	reply := f.replies[f.calls]
	f.calls++
	return reply, nil
}
func (f *sequencedLLM) Available() bool   { return true }
func (f *sequencedLLM) ModelName() string { return "fake" }
func (f *sequencedLLM) Backend() string   { return "fake" }

func TestSummarizeWithLLM_EmptyReplyThenSuccess_Retries(t *testing.T) {
	orig := emptyReplyRetrySleep
	emptyReplyRetrySleep = func(time.Duration) {} // no real wait in tests
	defer func() { emptyReplyRetrySleep = orig }()

	fake := &sequencedLLM{replies: []string{"", `{"summary": "CPU 過載", "confidence": "high", "escalate": false, "reason": "x"}`}}
	alert := Alert{Labels: map[string]string{"alertname": "cpu_high", "host": "h"}, Status: "firing"}

	result, err := SummarizeWithLLM(fake, alert, nil, "")
	if err != nil {
		t.Fatalf("SummarizeWithLLM: %v", err)
	}
	if fake.calls != 2 {
		t.Errorf("Chat called %d times, want exactly 2 (one empty, one retry)", fake.calls)
	}
	if result.Summary != "CPU 過載" {
		t.Errorf("unexpected result: %+v", result)
	}
}

func TestSummarizeWithLLM_EmptyReplyTwice_Fails(t *testing.T) {
	orig := emptyReplyRetrySleep
	emptyReplyRetrySleep = func(time.Duration) {}
	defer func() { emptyReplyRetrySleep = orig }()

	fake := &sequencedLLM{replies: []string{"", ""}}
	alert := Alert{Labels: map[string]string{"alertname": "cpu_high", "host": "h"}, Status: "firing"}

	_, err := SummarizeWithLLM(fake, alert, nil, "")
	if err == nil {
		t.Fatal("expected an error when the LLM returns an empty reply twice in a row")
	}
	if fake.calls != 2 {
		t.Errorf("Chat called %d times, want exactly 2 (no third attempt)", fake.calls)
	}
}

func TestSummarizeWithLLM_ChatError_NoRetry(t *testing.T) {
	// A real transport/API error is a different failure mode from an
	// empty reply (Chat itself failed, vs. Chat succeeded with nothing to
	// show) — chatWithEmptyReplyRetry's retry is specifically for the
	// latter and must not also retry a hard error.
	fake := &fakeLLM{err: errors.New("connection refused")}
	alert := Alert{Labels: map[string]string{"alertname": "cpu_high", "host": "h"}, Status: "firing"}

	_, err := SummarizeWithLLM(fake, alert, nil, "")
	if err == nil {
		t.Fatal("expected an error when Chat itself fails")
	}
}

func TestSummarizeWithLLM_NonEmptyFirstTry_NoRetry(t *testing.T) {
	fake := &sequencedLLM{replies: []string{`{"summary": "ok", "confidence": "high", "escalate": false, "reason": "x"}`}}
	alert := Alert{Labels: map[string]string{"alertname": "cpu_high", "host": "h"}, Status: "firing"}

	_, err := SummarizeWithLLM(fake, alert, nil, "")
	if err != nil {
		t.Fatalf("SummarizeWithLLM: %v", err)
	}
	if fake.calls != 1 {
		t.Errorf("Chat called %d times, want exactly 1 (no retry needed)", fake.calls)
	}
}
