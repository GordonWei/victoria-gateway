package aiops

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/model"
)

var fallbackAlert = Alert{Labels: map[string]string{"alertname": "cpu_high", "host": "h"}, Status: "firing"}

// okServer answers every chat completion with a fixed content string.
func okServer(t *testing.T, content string, calls *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			*calls++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":` + jsonString(content) + `}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func jsonString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func openAI(endpoint string, timeout time.Duration) model.LLM {
	return model.NewOpenAIClient(model.OpenAIClientConfig{Endpoint: endpoint, Model: "m", Backend: "test", Timeout: timeout})
}

const goodReply = `{"summary": "from fallback", "confidence": "high", "escalate": false, "reason": "x"}`

func TestSummarizerFallback_SwitchesOnNon2xx(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not loaded", http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	var calls int
	good := okServer(t, goodReply, &calls)

	s := NewSummarizerWithFallbacks([]SummarizerBackend{
		{Name: "summarizer", LLM: openAI(bad.URL, time.Second)},
		{Name: "summarizer.fallbacks[0]", LLM: openAI(good.URL, time.Second)},
	})
	res, err := s.Summarize(fallbackAlert, nil, "")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if res.Summary != "from fallback" || calls != 1 {
		t.Errorf("res = %+v, fallback calls = %d", res, calls)
	}
}

func TestSummarizerFallback_SwitchesOnConnectionRefused(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // nothing listens there any more
	good := okServer(t, goodReply, nil)

	s := NewSummarizerWithFallbacks([]SummarizerBackend{
		{Name: "summarizer", LLM: openAI(deadURL, time.Second)},
		{Name: "summarizer.fallbacks[0]", LLM: openAI(good.URL, time.Second)},
	})
	res, err := s.Summarize(fallbackAlert, nil, "")
	if err != nil || res.Summary != "from fallback" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestSummarizerFallback_SwitchesOnTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	good := okServer(t, goodReply, nil)

	s := NewSummarizerWithFallbacks([]SummarizerBackend{
		{Name: "summarizer", LLM: openAI(slow.URL, 100*time.Millisecond)},
		{Name: "summarizer.fallbacks[0]", LLM: openAI(good.URL, time.Second)},
	})
	res, err := s.Summarize(fallbackAlert, nil, "")
	if err != nil || res.Summary != "from fallback" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestSummarizerFallback_ParseFailedDoesNotSwitch(t *testing.T) {
	var fbCalls int
	primary := okServer(t, "not json at all", nil)
	fb := okServer(t, goodReply, &fbCalls)

	s := NewSummarizerWithFallbacks([]SummarizerBackend{
		{Name: "summarizer", LLM: openAI(primary.URL, time.Second)},
		{Name: "summarizer.fallbacks[0]", LLM: openAI(fb.URL, time.Second)},
	})
	res, err := s.Summarize(fallbackAlert, nil, "")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if !res.ParseFailed || res.Summary != "not json at all" || fbCalls != 0 {
		t.Errorf("res = %+v, fallback calls = %d; want the primary's raw reply and no fallback call", res, fbCalls)
	}
}

func TestSummarizerFallback_EmptyReplyTwiceDoesNotSwitch(t *testing.T) {
	orig := emptyReplyRetrySleep
	emptyReplyRetrySleep = func(time.Duration) {}
	defer func() { emptyReplyRetrySleep = orig }()

	fb := &fakeLLM{reply: goodReply}
	s := NewSummarizerWithFallbacks([]SummarizerBackend{
		{Name: "summarizer", LLM: &sequencedLLM{replies: []string{"", ""}}},
		{Name: "summarizer.fallbacks[0]", LLM: fb},
	})
	_, err := s.Summarize(fallbackAlert, nil, "")
	if err == nil || IsLLMUnavailable(err) {
		t.Fatalf("err = %v, want a non-unavailable error (the server answered, just emptily)", err)
	}
}

func TestSummarizerFallback_AllFailNamesEach(t *testing.T) {
	s := NewSummarizerWithFallbacks([]SummarizerBackend{
		{Name: "summarizer", LLM: &fakeLLM{err: errors.New("connection refused")}},
		{Name: "summarizer.fallbacks[0]", LLM: &fakeLLM{err: errors.New("503 busy")}},
	})
	_, err := s.Summarize(fallbackAlert, nil, "")
	if err == nil || !IsLLMUnavailable(err) {
		t.Fatalf("err = %v, want an unavailable error", err)
	}
	for _, want := range []string{"all 2 local summarizers failed", "summarizer: ", "connection refused", "summarizer.fallbacks[0]: ", "503 busy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, missing %q", err, want)
		}
	}
}

func TestSummarizer_SingleBackendKeepsErrorText(t *testing.T) {
	s := NewSummarizer(&fakeLLM{err: errors.New("connection refused")})
	_, err := s.Summarize(fallbackAlert, nil, "")
	if err == nil || err.Error() != "summarize: fake chat failed: connection refused" {
		t.Fatalf("err = %v, want the unchanged pre-fallback text", err)
	}
	if !IsLLMUnavailable(err) {
		t.Error("a single backend's chat failure should still count as unavailable")
	}
}
