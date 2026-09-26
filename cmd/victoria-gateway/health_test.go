package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/metrics"
)

func intp(v int) *int { return &v }

// newHungLLM accepts connections but never sends a response header — a
// Mac that went to sleep with the shim's socket still open.
func newHungLLM(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The production shape: one local summarizer with a long timeout_sec and
// a legacy cloud block. A hung summarizer must hand over to the cloud
// after the probe, not after timeout_sec.
func TestSummarizeOne_HungLocal_ProbeEscalatesWithoutWaitingTimeout(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	hung := newHungLLM(t)
	var cloudCalls int
	cloud := countingCloud(t, "cloud took over", http.StatusOK, &cloudCalls)
	logs := captureLog(t)

	h := newTestHandler(t, lokiSrv.URL, hung.URL)
	h.summarizer = buildSummarizer(config.LLMConfig{Endpoint: hung.URL, Model: "m", TimeoutSec: 180, ProbeTimeoutSec: intp(1)})
	h.cloud = anthropicAt(cloud.URL)
	h.legacyEscalations = []escalationStep{{display: "bedrock", llm: h.cloud}}
	h.metrics = &metrics.Counters{}

	start := time.Now()
	res := h.summarizeOne(testAlert())
	if res.Error != "" || res.AnalyzedBy != "cloud" || res.Summary != "cloud took over" {
		t.Fatalf("res = %+v", res)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("summarizeOne took %v, want roughly the 1s probe timeout", d)
	}
	wantContains(t, "log", logs(), "local summarizer unavailable", "health check failed", "no answer within 1s")
}

// A config that doesn't mention any of the new fields gets the breaker:
// after two unavailable alerts the local model is no longer contacted,
// and alerts still go to the cloud.
func TestSummarizeOne_DefaultBreaker_OpenLocalStillEscalates(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	var localRequests atomic.Int64
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localRequests.Add(1)
		http.Error(w, "no model loaded", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	var cloudCalls int
	cloud := countingCloud(t, "cloud took over", http.StatusOK, &cloudCalls)
	logs := captureLog(t)

	h := newTestHandler(t, lokiSrv.URL, down.URL)
	h.summarizer = buildSummarizer(config.LLMConfig{Endpoint: down.URL, Model: "m", TimeoutSec: 180})
	h.cloud = anthropicAt(cloud.URL)
	h.legacyEscalations = []escalationStep{{display: "bedrock", llm: h.cloud}}
	h.metrics = &metrics.Counters{}
	h.summarizer.SetObserver(h.metrics)

	for i := range 4 {
		res := h.summarizeOne(testAlert())
		if res.Error != "" || res.AnalyzedBy != "cloud" {
			t.Fatalf("alert %d: res = %+v, want the cloud's answer", i+1, res)
		}
	}
	if got := localRequests.Load(); got != 2 {
		t.Errorf("local requests = %d, want 2 (one probe per alert until the breaker opened)", got)
	}
	if cloudCalls != 4 {
		t.Errorf("cloud calls = %d, want 4", cloudCalls)
	}
	wantContains(t, "log", logs(), "circuit breaker for summarizer opened", "summarizer skipped: circuit breaker open")
	wantContains(t, "metrics", scrape(t, h.metrics),
		`victoria_gateway_local_llm_breaker_open{backend="summarizer"} 1`,
		`victoria_gateway_local_llm_skipped_total{backend="summarizer",reason="probe"} 2`,
		`victoria_gateway_local_llm_skipped_total{backend="summarizer",reason="breaker"} 2`,
		`victoria_gateway_escalations_total{target="bedrock"} 4`)
}
