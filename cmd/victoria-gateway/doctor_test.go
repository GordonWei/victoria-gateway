package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/model"
	"github.com/gordonwei/victoria-gateway/pkg/notify"
)

// Fake secrets: none of these may ever appear in doctor output.
const (
	docBotToken   = "123456:doctor-fake-bot-token"
	docGiteaToken = "doctor-fake-gitea-token"
	docDBPassword = "doctor-fake-db-pass"
	docAPIKey     = "doctor-fake-cloud-key"
	docAMPassword = "doctor-fake-am-pass"
)

type probeMode string

const (
	modeOK      probeMode = "ok"
	modeFail    probeMode = "fail"
	modeTimeout probeMode = "timeout"
)

// doctorBackends is one httptest server standing in for every HTTP
// dependency; each path family can be switched between answering,
// failing with 500, and hanging past the check's deadline.
type doctorBackends struct {
	srv   *httptest.Server
	mu    sync.Mutex
	modes map[string]probeMode
	hits  map[string]int
	auth  map[string]string // last Authorization header per family
}

func newDoctorBackends(t *testing.T) *doctorBackends {
	t.Helper()
	b := &doctorBackends{modes: map[string]probeMode{}, hits: map[string]int{}, auth: map[string]string{}}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fam := family(r.URL.Path)
		b.mu.Lock()
		mode := b.modes[fam]
		b.hits[fam]++
		b.auth[fam] = r.Header.Get("Authorization")
		b.mu.Unlock()
		switch mode {
		case modeFail:
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		case modeTimeout:
			select {
			case <-r.Context().Done():
			case <-time.After(1500 * time.Millisecond):
			}
			return
		}
		if fam == "embedding" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": []float32{0.1, 0.2, 0.3}}}})
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func family(path string) string {
	switch {
	case path == "/ready":
		return "loki"
	case path == "/v1/models":
		return "llm"
	case path == "/v1/embeddings":
		return "embedding"
	case strings.HasSuffix(path, "/getMe"):
		return "telegram"
	case strings.HasPrefix(path, "/api/v1/repos/"):
		return "tracker"
	case path == "/api/v2/status":
		return "alertmanager"
	}
	return "other:" + path
}

func (b *doctorBackends) set(fam string, m probeMode) {
	b.mu.Lock()
	b.modes[fam] = m
	b.mu.Unlock()
}

func (b *doctorBackends) hitCount(fam string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hits[fam]
}

type fakeDoctorStore struct {
	hasDup bool
	models []string
	err    error
}

func (s *fakeDoctorStore) HasDupOf(context.Context) (bool, error) { return s.hasDup, s.err }
func (s *fakeDoctorStore) DistinctEmbeddingModels(context.Context) ([]string, error) {
	return s.models, s.err
}
func (s *fakeDoctorStore) Close() error { return nil }

type countingLLM struct{ calls atomic.Int32 }

func (c *countingLLM) Chat([]model.Message, *model.ChatOptions) (string, error) {
	c.calls.Add(1)
	return "OK", nil
}
func (c *countingLLM) Available() bool   { return true }
func (c *countingLLM) ModelName() string { return "fake" }
func (c *countingLLM) Backend() string   { return "fake" }

type countingChannel struct {
	name string
	mu   sync.Mutex
	msgs []notify.Message
}

func (c *countingChannel) Name() string { return c.name }
func (c *countingChannel) Send(m notify.Message) error {
	c.mu.Lock()
	c.msgs = append(c.msgs, m)
	c.mu.Unlock()
	return nil
}

type fakeLogSource struct{ calls atomic.Int32 }

func (f *fakeLogSource) QueryRange(context.Context, aiops.LogIdentity, time.Time, time.Time, int) ([]aiops.LogEntry, error) {
	f.calls.Add(1)
	return nil, nil
}

// doctorRig wires a full config against the fake backends, with every
// side-effecting constructor counted.
type doctorRig struct {
	b         *doctorBackends
	cfg       *config.Config
	env       doctorEnv
	store     *fakeDoctorStore
	cloud     *countingLLM
	cloudN    atomic.Int32
	channels  []*countingChannel
	chanN     atomic.Int32
	logSrc    *fakeLogSource
	logSrcN   atomic.Int32
	storeOpen atomic.Int32
}

func newDoctorRig(t *testing.T) *doctorRig {
	t.Helper()
	b := newDoctorBackends(t)
	r := &doctorRig{b: b, store: &fakeDoctorStore{hasDup: true, models: []string{"bge-m3"}}, cloud: &countingLLM{}, logSrc: &fakeLogSource{}}
	r.cfg = &config.Config{
		Loki:       config.LokiConfig{Endpoint: b.srv.URL},
		Summarizer: config.LLMConfig{Endpoint: b.srv.URL, Model: "local-m"},
		Telegram:   config.TelegramConfig{BotToken: docBotToken, ChatID: 1},
		Cloud:      &config.CloudConfig{Provider: "gemini", APIKey: docAPIKey, Model: "g"},
		EscalationTargets: map[string]*config.CloudConfig{
			"investigate": {Provider: "aws-devops-agent"},
		},
		WebhookAuth:  &config.WebhookAuthConfig{Username: "am", Password: "doctor-fake-hook-pass"},
		WebUIAuth:    &config.WebhookAuthConfig{Username: "ui", Password: "doctor-fake-ui-pass"},
		Escalation:   config.EscalationConfig{MaxPerHour: 10},
		Alertmanager: &config.AlertmanagerConfig{Endpoint: b.srv.URL, Username: "am", Password: docAMPassword},
		RAG: &config.RAGConfig{
			Enabled:           true,
			MaskLogExcerpt:    true,
			PostgresDSN:       "postgres://vg:" + docDBPassword + "@db:5432/vg",
			EmbeddingEndpoint: b.srv.URL,
			EmbeddingModel:    "bge-m3",
			Gitea:             &config.GiteaConfig{Endpoint: b.srv.URL, Token: docGiteaToken, Owner: "ops-team", Repo: "incidents"},
		},
	}
	r.env = doctorEnv{
		http:        &http.Client{},
		telegramAPI: b.srv.URL,
		openStore: func(string) (doctorStore, error) {
			r.storeOpen.Add(1)
			return r.store, nil
		},
		buildCloud: func(string, *config.CloudConfig) (model.LLM, error) {
			r.cloudN.Add(1)
			return r.cloud, nil
		},
		buildLogSource: func(string, *config.LogSourceConfig, string) (aiops.LogSource, error) {
			r.logSrcN.Add(1)
			return r.logSrc, nil
		},
		newChannel: func(ch config.NotifyChannelConfig) (notify.Channel, error) {
			r.chanN.Add(1)
			c := &countingChannel{name: ch.Name}
			r.channels = append(r.channels, c)
			return c, nil
		},
		now: time.Now,
	}
	return r
}

func byCategory(results []checkResult, cat string) []checkResult {
	var out []checkResult
	for _, r := range results {
		if r.Category == cat {
			out = append(out, r)
		}
	}
	return out
}

func onlyResult(t *testing.T, results []checkResult, cat string) checkResult {
	t.Helper()
	rs := byCategory(results, cat)
	if len(rs) != 1 {
		t.Fatalf("category %q: got %d results %+v, want 1", cat, len(rs), rs)
	}
	return rs[0]
}

var quick = doctorOptions{timeout: 300 * time.Millisecond}

// Default run: every read-only probe answers OK, and nothing with a side
// effect or a bill is touched.
func TestDoctor_DefaultRun_AllOKAndNoSideEffects(t *testing.T) {
	r := newDoctorRig(t)
	results := runDoctorChecks(r.cfg, quick, r.env)

	for _, cat := range []string{"log source", "local model", "postgres", "embedding", "notify", "tracker", "alertmanager"} {
		if res := onlyResult(t, results, cat); res.Status != statusOK {
			t.Errorf("%s = %+v, want OK", cat, res)
		}
	}
	for _, res := range byCategory(results, "config") {
		t.Errorf("hardened config produced a warning: %+v", res)
	}
	cloud := byCategory(results, "cloud")
	if len(cloud) != 2 || cloud[0].Status != statusSkip || cloud[1].Status != statusSkip {
		t.Errorf("cloud = %+v, want two SKIPs", cloud)
	}
	if n := r.cloudN.Load(); n != 0 {
		t.Errorf("cloud targets built %d times without --probe-cloud", n)
	}
	if n := r.chanN.Load(); n != 0 {
		t.Errorf("notification channels built %d times without --send-test", n)
	}
	for _, fam := range []string{"loki", "llm", "embedding", "telegram", "tracker", "alertmanager"} {
		if n := r.b.hitCount(fam); n != 1 {
			t.Errorf("%s probed %d times, want 1", fam, n)
		}
	}
	if got := r.b.auth["tracker"]; got != "token "+docGiteaToken {
		t.Errorf("tracker probe Authorization = %q", got)
	}
}

// Each HTTP-probed dependency in its three states: answering, failing,
// hanging past the deadline.
func TestDoctor_ProbeStates(t *testing.T) {
	families := map[string]string{
		"loki":         "log source",
		"llm":          "local model",
		"embedding":    "embedding",
		"telegram":     "notify",
		"tracker":      "tracker",
		"alertmanager": "alertmanager",
	}
	for fam, cat := range families {
		for _, mode := range []probeMode{modeOK, modeFail, modeTimeout} {
			t.Run(fam+"/"+string(mode), func(t *testing.T) {
				r := newDoctorRig(t)
				r.b.set(fam, mode)
				opts := quick
				if mode == modeFail {
					// Room for a client's own retry/backoff before the
					// 500 is reported (the embedder retries once).
					opts.timeout = 5 * time.Second
				}
				res := onlyResult(t, runDoctorChecks(r.cfg, opts, r.env), cat)
				switch mode {
				case modeOK:
					if res.Status != statusOK {
						t.Errorf("got %+v, want OK", res)
					}
				case modeFail:
					if res.Status != statusFail || !strings.Contains(res.Detail, "500") {
						t.Errorf("got %+v, want FAIL mentioning 500", res)
					}
				case modeTimeout:
					if res.Status != statusFail || !strings.Contains(res.Detail, "timed out") && !strings.Contains(res.Detail, "deadline exceeded") {
						t.Errorf("got %+v, want a timeout FAIL", res)
					}
				}
			})
		}
	}
}

func TestDoctor_Postgres(t *testing.T) {
	r := newDoctorRig(t)
	r.store.hasDup = false
	r.store.models = []string{"other-model"}
	res := onlyResult(t, runDoctorChecks(r.cfg, quick, r.env), "postgres")
	if res.Status != statusWarn || !strings.Contains(res.Detail, "dup_of") || !strings.Contains(res.Detail, "other-model") {
		t.Errorf("got %+v, want WARN for missing dup_of and model drift", res)
	}

	r = newDoctorRig(t)
	r.env.openStore = func(dsn string) (doctorStore, error) {
		return nil, errors.New("ping postgres: cannot connect to " + dsn)
	}
	res = onlyResult(t, runDoctorChecks(r.cfg, quick, r.env), "postgres")
	if res.Status != statusFail {
		t.Errorf("got %+v, want FAIL", res)
	}

	r = newDoctorRig(t)
	r.env.openStore = func(string) (doctorStore, error) { time.Sleep(2 * time.Second); return r.store, nil }
	if res = onlyResult(t, runDoctorChecks(r.cfg, quick, r.env), "postgres"); res.Status != statusFail || !strings.Contains(res.Detail, "timed out") {
		t.Errorf("got %+v, want timeout FAIL", res)
	}
}

func TestDoctor_ProbeCloud(t *testing.T) {
	r := newDoctorRig(t)
	opts := quick
	opts.probeCloud = true
	cloud := byCategory(runDoctorChecks(r.cfg, opts, r.env), "cloud")
	if len(cloud) != 2 {
		t.Fatalf("cloud = %+v", cloud)
	}
	if cloud[0].Status != statusOK || r.cloud.calls.Load() != 1 {
		t.Errorf("gemini target = %+v, chat calls %d, want OK after one call", cloud[0], r.cloud.calls.Load())
	}
	if cloud[1].Status != statusSkip || !strings.Contains(cloud[1].Detail, "investigation") {
		t.Errorf("investigation target = %+v, want SKIP even with --probe-cloud", cloud[1])
	}
	if n := r.cloudN.Load(); n != 1 {
		t.Errorf("cloud builder called %d times, want 1 (investigation target never built)", n)
	}
}

func TestDoctor_SendTest(t *testing.T) {
	r := newDoctorRig(t)
	r.cfg.Notifications = &config.NotificationsConfig{Channels: []config.NotifyChannelConfig{
		{Name: "chat", Type: "webhook", URL: "https://hooks.example.com/secret-path"},
	}}
	// Without --send-test: the webhook channel is skipped, nothing built.
	notes := byCategory(runDoctorChecks(r.cfg, quick, r.env), "notify")
	if len(notes) != 2 || notes[0].Status != statusSkip || notes[1].Status != statusOK {
		t.Errorf("notify = %+v, want webhook SKIP + telegram getMe OK", notes)
	}
	if r.chanN.Load() != 0 {
		t.Fatal("channel built without --send-test")
	}

	opts := quick
	opts.sendTest = true
	notes = byCategory(runDoctorChecks(r.cfg, opts, r.env), "notify")
	if len(notes) != 2 || notes[0].Status != statusOK || notes[1].Status != statusOK {
		t.Errorf("notify = %+v, want two OK", notes)
	}
	if len(r.channels) != 2 {
		t.Fatalf("channels built = %d, want 2", len(r.channels))
	}
	for _, c := range r.channels {
		if len(c.msgs) != 1 || !strings.Contains(c.msgs[0].AlertName, "[TEST]") || !strings.Contains(c.msgs[0].Summary, "[TEST]") {
			t.Errorf("channel %s got %+v, want one [TEST] message", c.name, c.msgs)
		}
	}
}

func TestDoctor_CloudLogSources(t *testing.T) {
	r := newDoctorRig(t)
	r.cfg.LogSource = &config.LogSourceConfig{Type: "cloudwatch", CloudWatch: &config.CloudWatchConfig{Region: "us-east-1", LogGroupNames: []string{"/app"}}}
	res := onlyResult(t, runDoctorChecks(r.cfg, quick, r.env), "log source")
	if res.Status != statusSkip || r.logSrcN.Load() != 0 {
		t.Errorf("got %+v (built %d), want SKIP without building the source", res, r.logSrcN.Load())
	}
	opts := quick
	opts.probeCloudLogs = true
	res = onlyResult(t, runDoctorChecks(r.cfg, opts, r.env), "log source")
	if res.Status != statusOK || r.logSrc.calls.Load() != 1 {
		t.Errorf("got %+v (queries %d), want OK after one query", res, r.logSrc.calls.Load())
	}
}

func TestDoctor_SecurityWarningsBecomeWARN(t *testing.T) {
	r := newDoctorRig(t)
	r.cfg.WebhookAuth = nil
	r.cfg.Escalation.MaxPerHour = 0
	warns := byCategory(runDoctorChecks(r.cfg, quick, r.env), "config")
	if len(warns) != 2 || warns[0].Status != statusWarn || warns[1].Status != statusWarn {
		t.Errorf("config = %+v, want two WARN", warns)
	}
}

// End to end through doctor(): exit code and that no configured secret is
// printed, including the bot token that sits inside a failing request URL.
func TestDoctor_ExitCodeAndRedaction(t *testing.T) {
	r := newDoctorRig(t)
	cfgYAML := `
loki:
  endpoint: "` + r.b.srv.URL + `"
summarizer:
  endpoint: "` + r.b.srv.URL + `"
  model: "local-m"
telegram:
  bot_token: "` + docBotToken + `"
  chat_id: 1
alertmanager:
  endpoint: "` + r.b.srv.URL + `"
  username: "am"
  password: "` + docAMPassword + `"
cloud:
  provider: "gemini"
  api_key: "` + docAPIKey + `"
  model: "g"
rag:
  enabled: true
  postgres_dsn: "postgres://vg:` + docDBPassword + `@db:5432/vg"
  embedding_endpoint: "` + r.b.srv.URL + `"
  embedding_model: "bge-m3"
  gitea:
    endpoint: "` + r.b.srv.URL + `"
    token: "` + docGiteaToken + `"
    owner: "ops-team"
    repo: "incidents"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	// The store fails with the DSN in its message; Telegram's server is
	// gone, so the transport error quotes the getMe URL with the token.
	r.env.openStore = func(dsn string) (doctorStore, error) {
		return nil, errors.New("ping postgres: failed to connect to " + dsn)
	}
	r.env.telegramAPI = "http://127.0.0.1:1"

	var out bytes.Buffer
	code := doctor(path, quick, r.env, &out)
	text := out.String()
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (postgres and telegram fail)\n%s", code, text)
	}
	for _, secret := range []string{docBotToken, docGiteaToken, docDBPassword, docAPIKey, docAMPassword} {
		if strings.Contains(text, secret) {
			t.Errorf("output leaks %q:\n%s", secret, text)
		}
	}
	for _, want := range []string{"[OK  ] config", "[FAIL] postgres", "[FAIL] notify", "[WARN] config", "[SKIP] cloud", "FAIL,"} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}

	// All dependencies healthy and a hardened config: exit 0.
	r2 := newDoctorRig(t)
	hardened := strings.Replace(cfgYAML, "rag:\n", "webhook_auth:\n  username: am\n  password: hookpass1\nwebui_auth:\n  username: ui\n  password: uipass12\nescalation:\n  max_per_hour: 5\nrag:\n  mask_log_excerpt: true\n", 1)
	hardened = strings.ReplaceAll(hardened, r.b.srv.URL, r2.b.srv.URL)
	if err := os.WriteFile(path, []byte(hardened), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := doctor(path, quick, r2.env, &out); code != 0 {
		t.Errorf("healthy run exit code = %d, want 0\n%s", code, out.String())
	}
}

func TestDoctor_BadConfigFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("summarizer: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := doctor(path, quick, defaultDoctorEnv(), &out); code != 1 || !strings.Contains(out.String(), "[FAIL] config") {
		t.Errorf("exit %d, output:\n%s", code, out.String())
	}
}

func TestRedactor(t *testing.T) {
	cfg := &config.Config{
		Telegram: config.TelegramConfig{BotToken: "999:abc/def+ghi"},
		RAG:      &config.RAGConfig{PostgresDSN: "host=db user=vg password='kv-pass-1' dbname=vg"},
		Notifications: &config.NotificationsConfig{Channels: []config.NotifyChannelConfig{
			{Name: "slack", Type: "webhook", URL: "https://hooks.example.com/services/T1/B2/xyz123", Headers: map[string]string{"Authorization": "Bearer hdr-secret-1"}},
		}},
		MCP:             &config.MCPConfig{HTTP: &config.MCPHTTPConfig{BearerToken: "mcp-bearer-plain-value"}},
		TelegramActions: &config.TelegramActionsConfig{HMACSecret: "button-signing-plain-value"},
	}
	red := newRedactor(cfg)
	in := `Get "https://api.example/bot999:abc%2Fdef+ghi/getMe" | kv-pass-1 | https://hooks.example.com/services/T1/B2/xyz123 | hdr-secret-1 | password=other1234 | mcp-bearer-plain-value | button-signing-plain-value`
	got := red.redact(in)
	for _, s := range []string{"abc", "kv-pass-1", "xyz123", "hdr-secret-1", "other1234", "mcp-bearer-plain-value", "button-signing-plain-value"} {
		if strings.Contains(got, s) {
			t.Errorf("redacted text still contains %q: %s", s, got)
		}
	}
}
