package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/judge"
	"github.com/gordonwei/victoria-gateway/pkg/model"
)

// newFakeJudge returns a Jev API stand-in that always answers with the
// given escalate probability (severity is a fixed, unremarkable value —
// only escalate_probability matters to these tests).
func newFakeJudge(t *testing.T, escalateProbability float64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "jev-latest",
			"answers": map[string]any{
				"severity":         map[string]any{"type": "score", "score": 1.0, "confidence": 0.5},
				"needs_escalation": map[string]any{"type": "noul", "noul": escalateProbability},
			},
		})
	}))
}

// TestSummarizeOne_JudgeEscalates_LocalDidNot confirms Jev crossing the
// threshold turns a non-escalating alert into one that escalates to
// cloud, even though the local model's own self-report said not to and
// no always_cloud rule matches — this is the "Jev widens coverage" case.
func TestSummarizeOne_JudgeEscalates_LocalDidNot(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary": "local answer", "confidence": "high", "escalate": false, "reason": "clear"}`)
	defer llmSrv.Close()
	cloudSrv := newFakeCloud(t, "cloud answer")
	defer cloudSrv.Close()
	judgeSrv := newFakeJudge(t, 0.90)
	defer judgeSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	h.cloud = model.NewAnthropicClient(model.AnthropicClientConfig{Endpoint: cloudSrv.URL, APIKey: "k", Model: "m"})
	h.judgeClient = judge.NewClientWithEndpoint("test-key", judgeSrv.URL)
	h.judgeEscalateThreshold = 0.70

	alert := aiops.Alert{
		Status:   "firing",
		Labels:   map[string]string{"alertname": "cpu_high", "host": "test-host"},
		StartsAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	}
	res := h.summarizeOne(alert)

	if res.AnalyzedBy != "cloud" {
		t.Errorf("AnalyzedBy = %q, want %q (Jev crossing threshold should escalate on its own)", res.AnalyzedBy, "cloud")
	}
}

// TestSummarizeOne_JudgeUnreachable_LocalStillEscalates confirms a
// down/erroring Jev endpoint never blocks an escalation the existing
// local self-report already decided on.
func TestSummarizeOne_JudgeUnreachable_LocalStillEscalates(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary": "local guess", "confidence": "low", "escalate": true, "reason": "not sure"}`)
	defer llmSrv.Close()
	cloudSrv := newFakeCloud(t, "cloud answer")
	defer cloudSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	h.cloud = model.NewAnthropicClient(model.AnthropicClientConfig{Endpoint: cloudSrv.URL, APIKey: "k", Model: "m"})
	// Points at a closed port — every call fails.
	h.judgeClient = judge.NewClientWithEndpoint("test-key", "http://127.0.0.1:1")
	h.judgeEscalateThreshold = 0.70

	alert := aiops.Alert{
		Status:   "firing",
		Labels:   map[string]string{"alertname": "cpu_high", "host": "test-host"},
		StartsAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	}
	res := h.summarizeOne(alert)

	if res.AnalyzedBy != "cloud" {
		t.Errorf("AnalyzedBy = %q, want %q (local's own escalate signal must survive a failed Jev call)", res.AnalyzedBy, "cloud")
	}
}

// TestSummarizeOne_JudgeBelowThreshold_StaysLocal confirms Jev not
// crossing the threshold, with nothing else asking to escalate, leaves
// the alert on the local result.
func TestSummarizeOne_JudgeBelowThreshold_StaysLocal(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary": "local answer", "confidence": "high", "escalate": false, "reason": "clear"}`)
	defer llmSrv.Close()
	judgeSrv := newFakeJudge(t, 0.40)
	defer judgeSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	h.judgeClient = judge.NewClientWithEndpoint("test-key", judgeSrv.URL)
	h.judgeEscalateThreshold = 0.70

	alert := aiops.Alert{
		Status:   "firing",
		Labels:   map[string]string{"alertname": "cpu_high", "host": "test-host"},
		StartsAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	}
	res := h.summarizeOne(alert)

	if res.AnalyzedBy != "local" {
		t.Errorf("AnalyzedBy = %q, want %q (Jev below threshold, nothing else asked to escalate)", res.AnalyzedBy, "local")
	}
}

func TestCombineEscalation(t *testing.T) {
	const threshold = 0.70

	tests := []struct {
		name           string
		existing       bool
		existingReason string
		judgment       judge.EscalationJudgment
		judgeErr       error
		wantEscalate   bool
		wantReasonHas  string // substring the reason must contain, "" means don't check
	}{
		{
			name:          "Jev says escalate, existing does not -> escalates",
			existing:      false,
			judgment:      judge.EscalationJudgment{EscalateProbability: 0.85},
			wantEscalate:  true,
			wantReasonHas: "Jev escalate_probability",
		},
		{
			name:           "Jev says no, existing already escalates -> still escalates unchanged",
			existing:       true,
			existingReason: "local model requested escalation",
			judgment:       judge.EscalationJudgment{EscalateProbability: 0.10},
			wantEscalate:   true,
			wantReasonHas:  "local model requested escalation",
		},
		{
			name:           "Jev call fails, existing already escalates -> stays escalated, not blocked",
			existing:       true,
			existingReason: "alertname is in the always_cloud list",
			judgeErr:       errors.New("judge: request failed: context deadline exceeded"),
			wantEscalate:   true,
			wantReasonHas:  "always_cloud",
		},
		{
			name:         "Jev call fails, existing does not escalate -> falls back to no escalation, not stuck/blocked",
			existing:     false,
			judgeErr:     errors.New("judge: api.typesafe.ai returned 401: invalid api key"),
			wantEscalate: false,
		},
		{
			name:         "Jev below threshold, existing does not escalate -> no escalation",
			existing:     false,
			judgment:     judge.EscalationJudgment{EscalateProbability: 0.50},
			wantEscalate: false,
		},
		{
			name:         "Jev exactly at threshold -> escalates (>=, not >)",
			existing:     false,
			judgment:     judge.EscalationJudgment{EscalateProbability: threshold},
			wantEscalate: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotEscalate, gotReason := combineEscalation(tt.existing, tt.existingReason, tt.judgment, tt.judgeErr, threshold)
			if gotEscalate != tt.wantEscalate {
				t.Errorf("escalate = %v, want %v (reason=%q)", gotEscalate, tt.wantEscalate, gotReason)
			}
			if tt.wantReasonHas != "" && !strings.Contains(gotReason, tt.wantReasonHas) {
				t.Errorf("reason = %q, want it to contain %q", gotReason, tt.wantReasonHas)
			}
		})
	}
}
