package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/suppress"
)

// fakeAlertmanager is a minimal httptest-backed Alertmanager v2 silence
// API for applySuppressionSilences' tests — no active silences exist
// unless preloaded, and every create is recorded so tests can assert
// whether --apply-silences actually called the API or not.
func fakeAlertmanager(t *testing.T, preloadActive bool) (*httptest.Server, *int32) {
	t.Helper()
	var createCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/silences":
			w.Header().Set("Content-Type", "application/json")
			if preloadActive {
				_, _ = w.Write([]byte(`[{"matchers":[{"name":"alertname","value":"SSLCertExpiringSoon","isRegex":false,"isEqual":true},{"name":"host","value":"web01","isRegex":false,"isEqual":true}],"status":{"state":"active"}}]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/silences":
			atomic.AddInt32(&createCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"silenceID":"new-silence-id"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, &createCalls
}

func testCandidate() suppress.Candidate {
	return suppress.Candidate{
		AlertName:        "SSLCertExpiringSoon",
		Host:             "web01",
		Count:            5,
		FirstConfirmed:   time.Now().Add(-30 * 24 * time.Hour),
		LastConfirmed:    time.Now(),
		SampleResolution: "known, self-signed cert on a dev box",
	}
}

func TestApplySuppressionSilences_DryRun_DoesNotCreate(t *testing.T) {
	server, createCalls := fakeAlertmanager(t, false)
	cfg := &config.Config{
		RAG:          &config.RAGConfig{Enabled: true},
		Alertmanager: &config.AlertmanagerConfig{Endpoint: server.URL},
	}

	applySuppressionSilences(cfg, []suppress.Candidate{testCandidate()}, "720h", false /* yes */)

	if got := atomic.LoadInt32(createCalls); got != 0 {
		t.Errorf("POST /api/v2/silences called %d times, want 0 (dry run without --yes)", got)
	}
}

func TestApplySuppressionSilences_Yes_Creates(t *testing.T) {
	server, createCalls := fakeAlertmanager(t, false)
	cfg := &config.Config{
		RAG:          &config.RAGConfig{Enabled: true},
		Alertmanager: &config.AlertmanagerConfig{Endpoint: server.URL},
	}

	applySuppressionSilences(cfg, []suppress.Candidate{testCandidate()}, "720h", true /* yes */)

	if got := atomic.LoadInt32(createCalls); got != 1 {
		t.Errorf("POST /api/v2/silences called %d times, want 1", got)
	}
}

func TestApplySuppressionSilences_ExistingActiveSilence_SkipsCreate(t *testing.T) {
	server, createCalls := fakeAlertmanager(t, true /* preload an active silence matching testCandidate() */)
	cfg := &config.Config{
		RAG:          &config.RAGConfig{Enabled: true},
		Alertmanager: &config.AlertmanagerConfig{Endpoint: server.URL},
	}

	applySuppressionSilences(cfg, []suppress.Candidate{testCandidate()}, "720h", true /* yes */)

	if got := atomic.LoadInt32(createCalls); got != 0 {
		t.Errorf("POST /api/v2/silences called %d times, want 0 (an active silence already matches)", got)
	}
}
