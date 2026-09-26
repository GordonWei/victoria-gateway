package main

import (
	"net/http"
	"net/http/httptest"
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
