package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/metrics"
	"github.com/gordonwei/victoria-gateway/pkg/notify"
)

// The README's Slack / Teams channel config, loaded from YAML the way a
// deployment would, routed through buildNotifier and delivered to local
// servers. Guards the YAML block-scalar form as well as the templates.
func TestChatWebhookChannels_FromYAMLConfig(t *testing.T) {
	var mu sync.Mutex
	got := map[string][]byte{}
	handlerFor := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			got[name] = b
			mu.Unlock()
		}
	}
	slack := httptest.NewServer(handlerFor("slack"))
	defer slack.Close()
	teams := httptest.NewServer(handlerFor("teams"))
	defer teams.Close()

	yml := `
loki: {endpoint: "http://loki:3100"}
summarizer: {endpoint: "http://llm:1234", model: "m"}
notifications:
  channels:
    - name: "slack-ops"
      type: webhook
      url: "` + slack.URL + `"
      body_template: '{"text": {{json .Text}}}'
    - name: "teams-ops"
      type: webhook
      url: "` + teams.URL + `"
      body_template: |
        {"type":"message","attachments":[{"contentType":"application/vnd.microsoft.card.adaptive",
        "content":{"type":"AdaptiveCard","$schema":"http://adaptivecards.io/schemas/adaptive-card.json",
        "version":"1.4","body":[{"type":"TextBlock","text":{{json .Text}},"wrap":true}]}}]}
  routes:
    - default: true
      channels: ["slack-ops", "teams-ops"]
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	router, err := buildNotifier(cfg, &metrics.Counters{})
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}

	router.Dispatch(notify.Message{AlertName: "Disk", Host: "web01", Summary: "line1\n\"quoted\" <b>", AnalyzedBy: "local"}, nil)

	mu.Lock()
	defer mu.Unlock()
	var s struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(got["slack"], &s); err != nil || !strings.Contains(s.Text, "\"quoted\" <b>") {
		t.Errorf("slack body = %s (err %v)", got["slack"], err)
	}
	if !json.Valid(got["teams"]) || !strings.Contains(string(got["teams"]), "AdaptiveCard") || !strings.Contains(string(got["teams"]), `\"quoted\"`) {
		t.Errorf("teams body = %s", got["teams"])
	}
}
