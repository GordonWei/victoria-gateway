package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/metrics"
	"github.com/gordonwei/victoria-gateway/pkg/model"
	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

const (
	secretInLog     = "hunter2secret"   // in a log line
	secretInAnnot   = "topsecret99"     // in an alert annotation
	secretInSummary = "abcd1234efgh"    // in what the model writes back
	secretInPlan    = "planpass5678xyz" // in a mitigation plan
)

// maskingLoki serves one log line that carries a credential.
func maskingLoki(t *testing.T) *httptest.Server {
	t.Helper()
	return maskingLokiLine(t, "login failed password="+secretInLog+" from 203.0.113.5")
}

// maskingLokiLine serves line as the only log line.
func maskingLokiLine(t *testing.T, line string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{"status": "success", "data": map[string]any{"resultType": "streams", "result": []map[string]any{{
			"stream": map[string]string{"host": "db1"},
			"values": [][]string{{fmt.Sprint(time.Now().UnixNano()), line}},
		}}}}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// bodyRecorder collects every request body an httptest server receives.
type bodyRecorder struct {
	mu     sync.Mutex
	bodies []string
}

func (b *bodyRecorder) add(r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	b.mu.Lock()
	b.bodies = append(b.bodies, string(raw))
	b.mu.Unlock()
}

func (b *bodyRecorder) all() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.bodies, "\n")
}

// recordingCloud is an escalation target that remembers the prompt it was
// sent and answers with a summary and a plan that quote credentials.
type recordingCloud struct {
	mu     sync.Mutex
	prompt string
}

func (c *recordingCloud) Chat(m []model.Message, o *model.ChatOptions) (string, error) {
	r, err := c.ChatDetailed(m, o)
	return r.Reply, err
}
func (c *recordingCloud) ChatDetailed(m []model.Message, _ *model.ChatOptions) (model.ChatResult, error) {
	var b strings.Builder
	for _, msg := range m {
		b.WriteString(msg.Content)
		b.WriteString("\n")
	}
	c.mu.Lock()
	c.prompt = b.String()
	c.mu.Unlock()
	return model.ChatResult{
		Reply:      "cloud says token=" + secretInSummary,
		Mitigation: planOK("rotate it: password=" + secretInPlan),
	}, nil
}
func (c *recordingCloud) Available() bool   { return true }
func (c *recordingCloud) ModelName() string { return "rec" }
func (c *recordingCloud) Backend() string   { return "rec" }

type maskingRig struct {
	h       *handler
	llmReqs *bodyRecorder
	embReqs *bodyRecorder
	store   *fakeRAGStore
	tr      *fakeTracker
	cloud   *recordingCloud
}

func newMaskingRig(t *testing.T, maskOn bool) *maskingRig {
	t.Helper()
	return newMaskingRigWithLoki(t, maskOn, maskingLoki(t))
}

func newMaskingRigWithLoki(t *testing.T, maskOn bool, loki *httptest.Server) *maskingRig {
	t.Helper()
	rig := &maskingRig{llmReqs: &bodyRecorder{}, embReqs: &bodyRecorder{}, cloud: &recordingCloud{}}

	reply, _ := json.Marshal(map[string]any{"summary": "local saw token=" + secretInSummary, "confidence": "high", "escalate": false, "reason": "x"})
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.llmReqs.add(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": string(reply)}}}})
	}))
	t.Cleanup(llm.Close)
	emb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.embReqs.add(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": []float32{0.1}}}})
	}))
	t.Cleanup(emb.Close)

	h := newTestHandler(t, loki.URL, llm.URL)
	h.metrics = &metrics.Counters{}
	h.maskLogExcerpt = maskOn
	h.cloud = rig.cloud
	rig.store = &fakeRAGStore{}
	rig.tr = &fakeTracker{nextIssue: 6}
	h.rag = rig.store
	h.ragEmbedder = rag.NewEmbedder(emb.URL, "bge-m3", "")
	h.tracker = rig.tr
	h.issueURL = func(n int64) string { return fmt.Sprintf("https://git.example/issues/%d", n) }
	rig.h = h
	return rig
}

func maskingAlert() aiops.Alert {
	return aiops.Alert{
		Status:      "firing",
		Labels:      map[string]string{"alertname": "DBAuthFailures", "host": "db1"},
		Annotations: map[string]string{"description": "app keeps sending password=" + secretInAnnot},
		StartsAt:    time.Now().Add(-time.Minute).Format(time.RFC3339),
	}
}

func assertNone(t *testing.T, what, got string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(got, s) {
			t.Errorf("%s still contains %q:\n%s", what, s, got)
		}
	}
}

// With the switch on, a credential in a log line or an annotation never
// reaches the local model, the embedder, the stored record or the issue,
// and one the model writes back never reaches the notification, the
// record or the issue either.
func TestMasking_On_LocalPath_NothingLeavesRaw(t *testing.T) {
	rig := newMaskingRig(t, true)
	alert := maskingAlert()

	res := rig.h.summarizeOne(alert)

	if res.Error != "" || res.AnalyzedBy != "local" {
		t.Fatalf("res = %+v, want a clean local analysis", res)
	}
	assertNone(t, "local model prompt", rig.llmReqs.all(), secretInLog, secretInAnnot)
	assertNone(t, "embedding input", rig.embReqs.all(), secretInLog, secretInAnnot, secretInSummary)
	assertNone(t, "notification summary", res.Summary, secretInSummary)
	assertNone(t, "stored log excerpt", rig.store.lastPending.LogExcerpt, secretInLog)
	assertNone(t, "stored summary", rig.store.lastPending.Summary, secretInSummary)
	if len(rig.tr.bodies) != 1 {
		t.Fatalf("issues = %d, want 1", len(rig.tr.bodies))
	}
	assertNone(t, "issue body", rig.tr.bodies[0], secretInLog, secretInSummary)

	// The redaction keeps the readable part: key names, the IP, the shape.
	wantContains(t, "local model prompt", rig.llmReqs.all(), "password=", "203.0.113.5")
	// The caller's alert is not edited in place.
	if got := alert.Annotations["description"]; !strings.Contains(got, secretInAnnot) {
		t.Errorf("maskInputs edited the caller's annotations: %q", got)
	}
}

func TestMasking_On_CloudPath_PromptSummaryAndPlan(t *testing.T) {
	rig := newMaskingRig(t, true)
	rig.h.escalation = config.EscalationConfig{AlwaysCloud: []string{"DBAuthFailures"}}

	res := rig.h.summarizeOne(maskingAlert())

	if res.Error != "" || res.AnalyzedBy != "cloud" {
		t.Fatalf("res = %+v, want a cloud analysis", res)
	}
	assertNone(t, "cloud prompt", rig.cloud.prompt, secretInLog, secretInAnnot)
	assertNone(t, "notification summary", res.Summary, secretInSummary)
	assertNone(t, "stored summary", rig.store.lastPending.Summary, secretInSummary)
	assertNone(t, "issue body (summary and plan)", rig.tr.bodies[0], secretInSummary, secretInPlan, secretInLog)
	wantContains(t, "issue body", rig.tr.bodies[0], "## 建議處置") // the plan is still there, just redacted
}

// The default is unchanged: with the switch off nothing is rewritten.
func TestMasking_Off_ByDefault_NothingRewritten(t *testing.T) {
	rig := newMaskingRig(t, false)

	res := rig.h.summarizeOne(maskingAlert())

	wantContains(t, "local model prompt", rig.llmReqs.all(), secretInLog, secretInAnnot)
	wantContains(t, "stored log excerpt", rig.store.lastPending.LogExcerpt, secretInLog)
	wantContains(t, "notification summary", res.Summary, secretInSummary)
}

// Each credential shape that has no "key=" to anchor on is carried
// through every exit, not just checked in pkg/mask: the local prompt, the
// embedding input, the stored excerpt and the issue body. The secret
// parts are assembled from pieces so this file passes cmd/leakcheck.
func TestMasking_On_PrefixedCredentials_EveryExit(t *testing.T) {
	cases := []struct{ name, line, secret string }{
		{"aws key id", "client " + "AK" + "IA" + "QWERTYUIOPASDFGH" + " denied", "QWERTYUIOPASDFGH"},
		{"github token", "clone with " + "gh" + "p_" + strings.Repeat("Zq7", 12), strings.Repeat("Zq7", 12)},
		{"jwt", "auth " + "ey" + "JhbGciOiJIUzI1NiJ9." + "ey" + "JzdWIiOiJib2IifQ.Zm9vYmFyYmF6cXV4", "Zm9vYmFyYmF6cXV4"},
		{"dsn password", "dial postgres://app:Pw9xQ2zz@db:5432/app refused", "Pw9xQ2zz"},
		{"pem key", "-----BEGIN EC " + "PRIVATE KEY----- MHcCAQEEIBkg4LVWM9nuwNSk", "MHcCAQEEIBkg4LVWM9nuwNSk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newMaskingRigWithLoki(t, true, maskingLokiLine(t, tc.line))
			res := rig.h.summarizeOne(maskingAlert())
			if res.Error != "" {
				t.Fatalf("res = %+v", res)
			}
			assertNone(t, "local model prompt", rig.llmReqs.all(), tc.secret)
			assertNone(t, "embedding input", rig.embReqs.all(), tc.secret)
			assertNone(t, "stored log excerpt", rig.store.lastPending.LogExcerpt, tc.secret)
			if len(rig.tr.bodies) != 1 {
				t.Fatalf("issues = %d, want 1", len(rig.tr.bodies))
			}
			assertNone(t, "issue body", rig.tr.bodies[0], tc.secret)

			// Not vacuous: with the switch off the same secret does reach
			// the prompt and the stored excerpt.
			off := newMaskingRigWithLoki(t, false, maskingLokiLine(t, tc.line))
			off.h.summarizeOne(maskingAlert())
			wantContains(t, "unmasked prompt", off.llmReqs.all(), tc.secret)
			wantContains(t, "unmasked excerpt", off.store.lastPending.LogExcerpt, tc.secret)
		})
	}
}
