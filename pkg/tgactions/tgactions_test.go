package tgactions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const secret = "test-secret-0123456789abcdef0123456789"

func TestSigner_RoundTripAndTamper(t *testing.T) {
	s := NewSigner(secret)
	data := s.Sign("AAAAAAAAAAAA")
	if len(data) > 64 {
		t.Fatalf("callback data is %d bytes, Telegram allows 64", len(data))
	}
	if n, err := s.Verify(data); err != nil || n != "AAAAAAAAAAAA" {
		t.Fatalf("Verify(own) = %q, %v", n, err)
	}
	parts := strings.Split(data, ":")
	for name, bad := range map[string]string{
		"other nonce, same mac": "v1:BBBBBBBBBBBB:" + parts[2],
		"flipped mac":           parts[0] + ":" + parts[1] + ":" + flip(parts[2]),
		"wrong version":         "v2:" + parts[1] + ":" + parts[2],
		"missing mac":           "v1:" + parts[1],
		"extra field":           data + ":x",
		"short nonce":           "v1:AAA:" + parts[2],
		"empty":                 "",
		"other secret":          NewSigner("another-secret-0123456789abcdef").Sign("AAAAAAAAAAAA"),
	} {
		if _, err := s.Verify(bad); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: Verify(%q) err = %v, want ErrBadSignature", name, bad, err)
		}
	}
}

func flip(s string) string {
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestStore_SingleUseExpiryRelease(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	s := NewStore(time.Hour, 100, c.now)
	nonces, err := s.Issue([]Kind{KindAck, KindSilence}, "payload")
	if err != nil || len(nonces) != 2 || nonces[KindAck] == nonces[KindSilence] {
		t.Fatalf("Issue = %v, %v", nonces, err)
	}
	k, p, err := s.Claim(nonces[KindAck])
	if err != nil || k != KindAck || p != "payload" {
		t.Fatalf("first claim = %v %v %v", k, p, err)
	}
	if _, _, err := s.Claim(nonces[KindAck]); !errors.Is(err, ErrUsed) {
		t.Fatalf("second claim err = %v, want ErrUsed", err)
	}
	s.Release(nonces[KindAck])
	if _, _, err := s.Claim(nonces[KindAck]); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	if _, _, err := s.Claim("never-issued"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown nonce err = %v", err)
	}
	c.add(time.Hour)
	if _, _, err := s.Claim(nonces[KindSilence]); !errors.Is(err, ErrUnknown) {
		t.Fatalf("expired claim err = %v, want ErrUnknown", err)
	}
	s.Release(nonces[KindSilence]) // releasing an expired nonce does nothing
	if _, _, err := s.Claim(nonces[KindSilence]); !errors.Is(err, ErrUnknown) {
		t.Fatal("expired nonce revived by Release")
	}
	if _, err := s.Issue([]Kind{KindAck}, nil); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 1 {
		t.Fatalf("expired entries not pruned: %d left", s.Len())
	}
}

func TestStore_EvictsOldestWhenFull(t *testing.T) {
	s := NewStore(time.Hour, 4, nil)
	first, _ := s.Issue([]Kind{KindAck, KindEscalate, KindSilence}, 1)
	_, _ = s.Issue([]Kind{KindAck, KindEscalate, KindSilence}, 2)
	if s.Len() != 4 {
		t.Fatalf("Len = %d, want 4", s.Len())
	}
	if _, _, err := s.Claim(first[KindAck]); !errors.Is(err, ErrUnknown) {
		t.Fatalf("oldest not evicted: %v", err)
	}
	if _, _, err := s.Claim(first[KindSilence]); err != nil {
		t.Fatalf("newer entry of the first batch evicted too early: %v", err)
	}
}

func TestStore_ConcurrentClaimsOnlyOneWins(t *testing.T) {
	s := NewStore(time.Hour, 10, nil)
	n, _ := s.Issue([]Kind{KindEscalate}, nil)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.Claim(n[KindEscalate]); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d concurrent claims succeeded, want 1", wins.Load())
	}
}

// fakeBotAPI is a fake Telegram Bot API: it serves queued updates once
// from getUpdates and records every other call.
type fakeBotAPI struct {
	mu       sync.Mutex
	token    string
	updates  [][]Update // one batch per getUpdates call
	offsets  []int64
	answers  []string
	messages []map[string]any
	failNext int
	srv      *httptest.Server
}

func newFakeBotAPI(t *testing.T, token string) *fakeBotAPI {
	f := &fakeBotAPI{token: token}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBotAPI) serve(w http.ResponseWriter, r *http.Request) {
	prefix := "/bot" + f.token + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"ok":false,"description":"Unauthorized"}`)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext > 0 {
		f.failNext--
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"ok":false,"description":"Conflict: terminated by other getUpdates request"}`)
		return
	}
	switch strings.TrimPrefix(r.URL.Path, prefix) {
	case "getUpdates":
		f.offsets = append(f.offsets, int64(body["offset"].(float64)))
		var batch []Update
		if len(f.updates) > 0 {
			batch, f.updates = f.updates[0], f.updates[1:]
		}
		b, _ := json.Marshal(map[string]any{"ok": true, "result": batch})
		_, _ = w.Write(b)
	case "answerCallbackQuery":
		f.answers = append(f.answers, body["text"].(string))
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
	case "sendMessage":
		f.messages = append(f.messages, body)
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":99}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"ok":false,"description":"Not Found"}`)
	}
}

const botToken = "123456:fake-test-token"

func TestClient_ErrorsNeverQuoteTheToken(t *testing.T) {
	f := newFakeBotAPI(t, "a-different-token")
	c := NewClient(f.srv.URL, botToken)
	err := c.AnswerCallbackQuery(context.Background(), "q", "x")
	if err == nil || strings.Contains(err.Error(), botToken) {
		t.Fatalf("err = %v", err)
	}
	// Transport failure: the URL (with the token) must not be in the error.
	dead := NewClient("http://127.0.0.1:1", botToken)
	err = dead.SendMessage(context.Background(), 1, "x", 0)
	if err == nil || strings.Contains(err.Error(), botToken) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("transport err = %v", err)
	}
}

func TestClient_PollAdvancesOffsetAndBacksOff(t *testing.T) {
	f := newFakeBotAPI(t, botToken)
	f.failNext = 1 // first getUpdates fails like a 409 conflict
	f.updates = [][]Update{
		{{UpdateID: 10, CallbackQuery: &CallbackQuery{ID: "a"}}, {UpdateID: 11}},
		{{UpdateID: 12, CallbackQuery: &CallbackQuery{ID: "b"}}},
	}
	c := NewClient(f.srv.URL, botToken)
	ctx, cancel := context.WithCancel(context.Background())
	var got []string
	var slept []time.Duration
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Poll(ctx, 0, func(_ context.Context, cq CallbackQuery) {
			got = append(got, cq.ID)
			if len(got) == 2 {
				cancel()
			}
		}, func(_ context.Context, d time.Duration) { slept = append(slept, d) })
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Poll did not stop after ctx cancel")
	}
	if strings.Join(got, ",") != "a,b" {
		t.Fatalf("handled %v", got)
	}
	if len(slept) != 1 || slept[0] != time.Second {
		t.Fatalf("backoff sleeps %v", slept)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.offsets) < 2 || f.offsets[0] != 0 || f.offsets[1] != 12 {
		t.Fatalf("offsets %v, want [0 12 ...]", f.offsets)
	}
}

type procRig struct {
	p      *Processor
	api    *fakeBotAPI
	clk    *clock
	execs  []Kind
	audits []string
	counts []string
	out    Outcome
}

func newProcRig(t *testing.T) *procRig {
	r := &procRig{api: newFakeBotAPI(t, botToken), clk: &clock{t: time.Unix(2_000_000, 0)}, out: Outcome{Result: "ok", Toast: "done", Reply: "did it"}}
	r.p = &Processor{
		Signer:  NewSigner(secret),
		Store:   NewStore(time.Hour, 100, r.clk.now),
		Client:  NewClient(r.api.srv.URL, botToken),
		ChatID:  -1001,
		Allowed: map[int64]bool{42: true},
		Exec: func(_ context.Context, k Kind, _ any, _ User) Outcome {
			r.execs = append(r.execs, k)
			return r.out
		},
		Audit: func(_ context.Context, actor, action, target, detail string) {
			r.audits = append(r.audits, actor+"|"+action+"|"+detail)
		},
		Count: func(a, res string) { r.counts = append(r.counts, a+"/"+res) },
	}
	return r
}

func (r *procRig) button(t *testing.T, k Kind) string {
	n, err := r.p.Store.Issue([]Kind{k}, "p")
	if err != nil {
		t.Fatal(err)
	}
	return r.p.Signer.Sign(n[k])
}

func press(user, chat int64, data string) CallbackQuery {
	return CallbackQuery{ID: "cq", From: User{ID: user}, Message: &Message{MessageID: 5, Chat: Chat{ID: chat}}, Data: data}
}

func TestProcessor_Refusals(t *testing.T) {
	r := newProcRig(t)
	ctx := context.Background()
	data := r.button(t, KindAck)

	r.p.Handle(ctx, press(7, -1001, data)) // not on the allowlist
	r.p.Handle(ctx, press(42, -2002, data))
	r.p.Handle(ctx, CallbackQuery{ID: "cq", From: User{ID: 42}, Data: data}) // no message
	parts := strings.Split(data, ":")
	r.p.Handle(ctx, press(42, -1001, parts[0]+":"+parts[1]+":"+flip(parts[2])))
	r.p.Handle(ctx, press(42, -1001, "v1:not-a-nonce:xxxxxxxxxxxxxxxx"))
	if len(r.execs) != 0 {
		t.Fatalf("refused presses reached the executor: %v", r.execs)
	}
	wantCounts := []string{"denied/" + DeniedUser, "denied/" + DeniedChat, "denied/" + DeniedChat, "denied/" + DeniedSignature, "denied/" + DeniedSignature}
	if strings.Join(r.counts, ",") != strings.Join(wantCounts, ",") {
		t.Fatalf("counts %v, want %v", r.counts, wantCounts)
	}
	if len(r.audits) != 5 || !strings.HasPrefix(r.audits[0], "telegram:7|telegram.action_denied|reason=user_not_allowed") {
		t.Fatalf("audits %v", r.audits)
	}
	// The refused presses did not consume the button.
	r.p.Handle(ctx, press(42, -1001, data))
	if len(r.execs) != 1 {
		t.Fatal("button unusable after refusals")
	}
}

func TestProcessor_ReplayDoubleClickAndExpiry(t *testing.T) {
	r := newProcRig(t)
	ctx := context.Background()
	data := r.button(t, KindSilence)
	r.p.Handle(ctx, press(42, -1001, data))
	r.p.Handle(ctx, press(42, -1001, data)) // double click / replay
	if len(r.execs) != 1 {
		t.Fatalf("executor ran %d times, want 1", len(r.execs))
	}
	if r.counts[1] != "denied/"+DeniedReplay {
		t.Fatalf("second press counted as %q", r.counts[1])
	}

	old := r.button(t, KindAck)
	r.clk.add(time.Hour + time.Second)
	r.p.Handle(ctx, press(42, -1001, old))
	if len(r.execs) != 1 || r.counts[len(r.counts)-1] != "denied/"+DeniedExpired {
		t.Fatalf("expired press: execs=%v counts=%v", r.execs, r.counts)
	}

	r.api.mu.Lock()
	defer r.api.mu.Unlock()
	if len(r.api.answers) != 3 || len(r.api.messages) != 1 {
		t.Fatalf("answers=%v messages=%v", r.api.answers, r.api.messages)
	}
	if r.api.messages[0]["text"] != "did it" || r.api.messages[0]["chat_id"].(float64) != -1001 {
		t.Fatalf("reply = %v", r.api.messages[0])
	}
	if !strings.HasPrefix(r.audits[0], "telegram:42|telegram.silence|result=ok") {
		t.Fatalf("audit = %v", r.audits[0])
	}
}

func TestProcessor_ReleaseAllowsRetry(t *testing.T) {
	r := newProcRig(t)
	ctx := context.Background()
	data := r.button(t, KindEscalate)
	r.out = Outcome{Result: "rate_limited", Toast: "later", Release: true}
	r.p.Handle(ctx, press(42, -1001, data))
	r.out = Outcome{Result: "ok", Toast: "ok"}
	r.p.Handle(ctx, press(42, -1001, data))
	r.p.Handle(ctx, press(42, -1001, data))
	if len(r.execs) != 2 {
		t.Fatalf("executor ran %d times, want 2 (rate-limited, then ok, then refused)", len(r.execs))
	}
	want := "escalate/rate_limited,escalate/ok,denied/" + DeniedReplay
	if strings.Join(r.counts, ",") != want {
		t.Fatalf("counts %v, want %s", r.counts, want)
	}
}
