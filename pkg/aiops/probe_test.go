package aiops

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const primaryReply = `{"summary": "from primary", "confidence": "high", "escalate": false, "reason": "x"}`

var chatReply = `{"choices":[{"message":{"role":"assistant","content":` + jsonString(primaryReply) + `}}]}`

// modelsServer serves /v1/models with modelsHandler and answers chat
// completions with chatReply, counting both.
func modelsServer(t *testing.T, modelsHandler http.HandlerFunc, probes, chats *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			probes.Add(1)
			modelsHandler(w, r)
			return
		}
		chats.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatReply))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// hangUntilDone never answers — the "TCP accepted, no header" state of a
// sleeping host — until the request is cancelled.
func hangUntilDone(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }

func TestSummarizerProbe_HangingPrimarySkippedWithoutWaitingChatTimeout(t *testing.T) {
	var p1, c1 atomic.Int64
	primary := modelsServer(t, hangUntilDone, &p1, &c1)
	good := okServer(t, goodReply, nil)

	s := NewSummarizerWithFallbacks([]SummarizerBackend{
		// A chat timeout far longer than the test would ever wait.
		{Name: "summarizer", LLM: openAI(primary.URL, time.Minute), ProbeTimeout: 150 * time.Millisecond},
		{Name: "summarizer.fallbacks[0]", LLM: openAI(good.URL, time.Minute)},
	})
	start := time.Now()
	res, err := s.Summarize(fallbackAlert, nil, "")
	if err != nil || res.Summary != "from fallback" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Summarize took %v; the hanging primary should have been skipped after the 150ms probe", d)
	}
	if p1.Load() != 1 || c1.Load() != 0 {
		t.Errorf("primary probes = %d, chats = %d; want 1 probe and no chat", p1.Load(), c1.Load())
	}
}

func TestSummarizerProbe_503SkipsToFallback(t *testing.T) {
	var probes, chats atomic.Int64
	primary := modelsServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "loading", http.StatusServiceUnavailable)
	}, &probes, &chats)
	good := okServer(t, goodReply, nil)

	s := NewSummarizerWithFallbacks([]SummarizerBackend{
		{Name: "summarizer", LLM: openAI(primary.URL, time.Minute), ProbeTimeout: time.Second},
		{Name: "summarizer.fallbacks[0]", LLM: openAI(good.URL, time.Minute)},
	})
	res, err := s.Summarize(fallbackAlert, nil, "")
	if err != nil || res.Summary != "from fallback" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if chats.Load() != 0 {
		t.Errorf("primary chat called %d times after a 503 probe", chats.Load())
	}
}

// A 404 on /v1/models means the server doesn't implement the list, not
// that it's down: chat must still be tried.
func TestSummarizerProbe_404StillCallsChat(t *testing.T) {
	var probes, chats atomic.Int64
	primary := modelsServer(t, http.NotFound, &probes, &chats)

	s := NewSummarizerWithFallbacks([]SummarizerBackend{
		{Name: "summarizer", LLM: openAI(primary.URL, time.Minute), ProbeTimeout: time.Second},
	})
	for range 2 {
		res, err := s.Summarize(fallbackAlert, nil, "")
		if err != nil || res.Summary != "from primary" {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
	}
	if probes.Load() != 2 || chats.Load() != 2 {
		t.Errorf("probes = %d, chats = %d; want 2 and 2", probes.Load(), chats.Load())
	}
}

func TestSummarizerProbe_SingleBackendErrorIsUnavailable(t *testing.T) {
	var probes, chats atomic.Int64
	primary := modelsServer(t, hangUntilDone, &probes, &chats)

	s := NewSummarizerWithFallbacks([]SummarizerBackend{
		{Name: "summarizer", LLM: openAI(primary.URL, time.Minute), ProbeTimeout: 100 * time.Millisecond},
	})
	_, err := s.Summarize(fallbackAlert, nil, "")
	if !IsLLMUnavailable(err) {
		t.Fatalf("err = %v, want IsLLMUnavailable so the caller escalates", err)
	}
	if !strings.Contains(err.Error(), "health check failed") || !strings.Contains(err.Error(), "no answer within 100ms") {
		t.Errorf("err = %q, want it to say the health check timed out", err)
	}
	if chats.Load() != 0 {
		t.Errorf("chat called %d times", chats.Load())
	}
}

func TestSummarizerProbe_DisabledSendsNoProbe(t *testing.T) {
	var probes, chats atomic.Int64
	primary := modelsServer(t, hangUntilDone, &probes, &chats)

	s := NewSummarizerWithFallbacks([]SummarizerBackend{
		{Name: "summarizer", LLM: openAI(primary.URL, time.Minute), ProbeTimeout: 0},
	})
	res, err := s.Summarize(fallbackAlert, nil, "")
	if err != nil || res.Summary != "from primary" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if probes.Load() != 0 {
		t.Errorf("probe sent %d times with ProbeTimeout 0", probes.Load())
	}
}
