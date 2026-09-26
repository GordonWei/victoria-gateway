package aiops

import (
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a settable clock for stepping through a cooldown.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestBreaker_StateMachine(t *testing.T) {
	clk := newFakeClock()
	b := newBreaker(2, time.Minute)

	if ok, _, _ := b.allow(clk.Now()); !ok {
		t.Fatal("closed breaker refused a call")
	}
	if tr := b.record(clk.Now(), true); tr != transitionNone {
		t.Fatalf("first failure: transition %q, want none", tr)
	}
	b.allow(clk.Now())
	if tr := b.record(clk.Now(), true); tr != transitionOpen {
		t.Fatalf("second failure: transition %q, want open", tr)
	}

	clk.Advance(59 * time.Second)
	ok, retryIn, _ := b.allow(clk.Now())
	if ok || retryIn != time.Second {
		t.Fatalf("during cooldown: ok=%v retryIn=%v, want refused with 1s left", ok, retryIn)
	}

	clk.Advance(time.Second)
	ok, _, tr := b.allow(clk.Now())
	if !ok || tr != transitionHalfOpen {
		t.Fatalf("after cooldown: ok=%v transition=%q, want a half-open trial", ok, tr)
	}
	if ok, retryIn, _ := b.allow(clk.Now()); ok || retryIn != 0 {
		t.Fatalf("second call while the trial is in flight: ok=%v retryIn=%v, want refused", ok, retryIn)
	}
	if tr := b.record(clk.Now(), true); tr != transitionReopen {
		t.Fatalf("failed trial: transition %q, want reopen", tr)
	}
	if ok, _, _ := b.allow(clk.Now()); ok {
		t.Fatal("re-opened breaker let a call through immediately")
	}

	clk.Advance(time.Minute)
	if ok, _, tr := b.allow(clk.Now()); !ok || tr != transitionHalfOpen {
		t.Fatalf("second cooldown: ok=%v transition=%q", ok, tr)
	}
	if tr := b.record(clk.Now(), false); tr != transitionClose {
		t.Fatalf("successful trial: transition %q, want close", tr)
	}
	// Closed again with a fresh count: one failure doesn't reopen it.
	b.allow(clk.Now())
	if tr := b.record(clk.Now(), true); tr != transitionNone {
		t.Fatalf("first failure after close: transition %q, want none", tr)
	}
}

func TestBreaker_SuccessResetsCount(t *testing.T) {
	clk := newFakeClock()
	b := newBreaker(2, time.Minute)
	for _, unavailable := range []bool{true, false, true} {
		b.allow(clk.Now())
		if tr := b.record(clk.Now(), unavailable); tr != transitionNone {
			t.Fatalf("transition %q; failures separated by a success must not open the breaker", tr)
		}
	}
}

func TestBreaker_ZeroDisables(t *testing.T) {
	if newBreaker(0, time.Minute) != nil || newBreaker(2, 0) != nil {
		t.Error("a zero threshold or cooldown should mean no breaker")
	}
}

// switchableServer answers /v1/models and chat with 503 while down is
// set, normally otherwise, counting every request that reaches it.
type switchableServer struct {
	*httptest.Server
	down     atomic.Bool
	requests atomic.Int64
}

func newSwitchableServer(t *testing.T) *switchableServer {
	t.Helper()
	s := &switchableServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		if s.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatReply))
	}))
	t.Cleanup(s.Close)
	return s
}

func breakerSummarizer(t *testing.T, primaryURL, fallbackURL string, clk *fakeClock) *Summarizer {
	t.Helper()
	s := NewSummarizerWithFallbacks([]SummarizerBackend{
		{Name: "summarizer", LLM: openAI(primaryURL, 5*time.Second), ProbeTimeout: time.Second, BreakerFailures: 2, BreakerCooldown: 2 * time.Minute},
		{Name: "summarizer.fallbacks[0]", LLM: openAI(fallbackURL, 5*time.Second)},
	})
	s.now = clk.Now
	return s
}

func TestSummarizerBreaker_OpenSkipHalfOpenClose(t *testing.T) {
	logs := captureAiopsLog(t)
	clk := newFakeClock()
	primary := newSwitchableServer(t)
	primary.down.Store(true)
	var fbCalls int
	fallback := okServer(t, goodReply, &fbCalls)
	s := breakerSummarizer(t, primary.URL, fallback.URL, clk)

	summarize := func() SummarizeResult {
		t.Helper()
		res, err := s.Summarize(fallbackAlert, nil, "")
		if err != nil {
			t.Fatalf("Summarize: %v", err)
		}
		return res
	}

	// Two probe failures open the breaker.
	summarize()
	summarize()
	if got := primary.requests.Load(); got != 2 {
		t.Fatalf("primary requests = %d, want 2 probes", got)
	}
	if !strings.Contains(logs(), "circuit breaker for summarizer opened after 2 consecutive unavailable failure(s); skipping it for 2m0s") {
		t.Errorf("no open log line; got:\n%s", logs())
	}

	// Open: skipped with no probe and no chat.
	clk.Advance(time.Minute)
	if res := summarize(); res.Summary != "from fallback" {
		t.Fatalf("res = %+v", res)
	}
	if got := primary.requests.Load(); got != 2 {
		t.Fatalf("primary requests = %d while open, want no new ones", got)
	}
	if !strings.Contains(logs(), "summarizer skipped: circuit breaker open (next try in 1m0s)") {
		t.Errorf("fallback log doesn't say why the primary was skipped; got:\n%s", logs())
	}

	// Cooldown over, primary back: the trial alert closes the breaker.
	clk.Advance(time.Minute)
	primary.down.Store(false)
	if res := summarize(); res.Summary != "from primary" {
		t.Fatalf("trial res = %+v, want the primary's answer", res)
	}
	wantLog := []string{"circuit breaker for summarizer half-open after 2m0s", "circuit breaker for summarizer closed"}
	for _, w := range wantLog {
		if !strings.Contains(logs(), w) {
			t.Errorf("log missing %q; got:\n%s", w, logs())
		}
	}
	if res := summarize(); res.Summary != "from primary" {
		t.Fatalf("after close res = %+v", res)
	}
}

func TestSummarizerBreaker_FailedTrialReopens(t *testing.T) {
	logs := captureAiopsLog(t)
	clk := newFakeClock()
	primary := newSwitchableServer(t)
	primary.down.Store(true)
	fallback := okServer(t, goodReply, nil)
	s := breakerSummarizer(t, primary.URL, fallback.URL, clk)

	for range 2 {
		_, _ = s.Summarize(fallbackAlert, nil, "")
	}
	clk.Advance(2 * time.Minute)
	_, _ = s.Summarize(fallbackAlert, nil, "") // the trial, still down
	if got := primary.requests.Load(); got != 3 {
		t.Fatalf("primary requests = %d, want 2 + the trial's probe", got)
	}
	if !strings.Contains(logs(), "circuit breaker for summarizer re-opened: the trial alert failed too; skipping it for another 2m0s") {
		t.Errorf("no reopen log line; got:\n%s", logs())
	}
	clk.Advance(time.Minute)
	_, _ = s.Summarize(fallbackAlert, nil, "")
	if got := primary.requests.Load(); got != 3 {
		t.Errorf("primary requests = %d after reopening, want it skipped", got)
	}
}

// Every backend open: Summarize fails fast with an unavailable error, so
// the caller's escalate-to-cloud path still runs.
func TestSummarizerBreaker_AllOpenIsUnavailable(t *testing.T) {
	clk := newFakeClock()
	a, b := newSwitchableServer(t), newSwitchableServer(t)
	a.down.Store(true)
	b.down.Store(true)
	s := NewSummarizerWithFallbacks([]SummarizerBackend{
		{Name: "summarizer", LLM: openAI(a.URL, 5*time.Second), ProbeTimeout: time.Second, BreakerFailures: 1, BreakerCooldown: time.Minute},
		{Name: "summarizer.fallbacks[0]", LLM: openAI(b.URL, 5*time.Second), ProbeTimeout: time.Second, BreakerFailures: 1, BreakerCooldown: time.Minute},
	})
	s.now = clk.Now
	_, _ = s.Summarize(fallbackAlert, nil, "")
	before := a.requests.Load() + b.requests.Load()

	_, err := s.Summarize(fallbackAlert, nil, "")
	if !IsLLMUnavailable(err) {
		t.Fatalf("err = %v, want IsLLMUnavailable", err)
	}
	if !strings.Contains(err.Error(), "summarizer: summarize: summarizer skipped: circuit breaker open") ||
		!strings.Contains(err.Error(), "summarizer.fallbacks[0]: summarize: summarizer.fallbacks[0] skipped: circuit breaker open") {
		t.Errorf("err = %q, want each skip named", err)
	}
	if after := a.requests.Load() + b.requests.Load(); after != before {
		t.Errorf("%d requests sent while every breaker was open", after-before)
	}
}

// Only unavailable failures count: a 401 is our misconfiguration, and
// proves the server is there.
func TestSummarizerBreaker_ConfigErrorDoesNotCount(t *testing.T) {
	clk := newFakeClock()
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer srv.Close()
	s := NewSummarizerWithFallbacks([]SummarizerBackend{
		{Name: "summarizer", LLM: openAI(srv.URL, 5*time.Second), ProbeTimeout: time.Second, BreakerFailures: 1, BreakerCooldown: time.Minute},
	})
	s.now = clk.Now
	for range 3 {
		_, err := s.Summarize(fallbackAlert, nil, "")
		if err == nil || IsLLMUnavailable(err) {
			t.Fatalf("err = %v, want a non-unavailable 401 error", err)
		}
	}
	// Each alert: probe (401, inconclusive) + chat (401).
	if got := requests.Load(); got != 6 {
		t.Errorf("requests = %d, want 6 — the breaker must not open on 401s", got)
	}
}

// Turned off, a failing backend is tried on every alert, as before.
func TestSummarizerBreaker_DisabledTriesEveryTime(t *testing.T) {
	primary := newSwitchableServer(t)
	primary.down.Store(true)
	fallback := okServer(t, goodReply, nil)
	s := NewSummarizerWithFallbacks([]SummarizerBackend{
		{Name: "summarizer", LLM: openAI(primary.URL, 5*time.Second)},
		{Name: "summarizer.fallbacks[0]", LLM: openAI(fallback.URL, 5*time.Second)},
	})
	for range 5 {
		if _, err := s.Summarize(fallbackAlert, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := primary.requests.Load(); got != 5 {
		t.Errorf("primary requests = %d, want one chat per alert", got)
	}
}

// Many concurrent alerts after the cooldown: exactly one trial reaches
// the primary; the rest go straight to the fallback.
func TestSummarizerBreaker_ConcurrentHalfOpenLetsOneTrialThrough(t *testing.T) {
	clk := newFakeClock()
	release := make(chan struct{})
	var primaryChats atomic.Int64
	var down atomic.Bool
	down.Store(true)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/v1/models" {
			return
		}
		primaryChats.Add(1)
		<-release // hold the trial open while the others arrive
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatReply))
	}))
	defer primary.Close()
	var mu sync.Mutex
	var fbCalls int
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fbCalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + jsonString(goodReply) + `}}]}`))
	}))
	defer fallback.Close()
	s := breakerSummarizer(t, primary.URL, fallback.URL, clk)

	// Open it concurrently: race detector coverage for the closed path.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Summarize(fallbackAlert, nil, "")
		}()
	}
	wg.Wait()

	clk.Advance(2 * time.Minute)
	down.Store(false)

	trialDone := make(chan SummarizeResult)
	go func() {
		res, _ := s.Summarize(fallbackAlert, nil, "")
		trialDone <- res
	}()
	deadline := time.Now().Add(5 * time.Second)
	for primaryChats.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("trial never reached the primary")
		}
		time.Sleep(5 * time.Millisecond)
	}

	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.Summarize(fallbackAlert, nil, "")
			if err != nil || res.Summary != "from fallback" {
				t.Errorf("concurrent alert during trial: res = %+v, err = %v", res, err)
			}
		}()
	}
	wg.Wait()
	close(release)
	if res := <-trialDone; res.Summary != "from primary" {
		t.Errorf("trial res = %+v", res)
	}
	if got := primaryChats.Load(); got != 1 {
		t.Errorf("primary chats = %d, want only the trial", got)
	}
}

// captureAiopsLog redirects the standard logger for the rest of the test
// and returns a function yielding everything written so far.
func captureAiopsLog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf strings.Builder
	orig := log.Writer()
	log.SetOutput(lockedWriter{mu: &mu, w: &buf})
	t.Cleanup(func() { log.SetOutput(orig) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *strings.Builder
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
