package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/metrics"
)

func auditActions(a *fakeAuditLogger) []string {
	var out []string
	for _, e := range a.entries {
		out = append(out, e.Action)
	}
	return out
}

func TestEscalationAudit_SuccessfulEscalationIsRecorded(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary":"local","confidence":"low","escalate":true,"reason":"unsure"}`)
	defer llmSrv.Close()
	var calls int
	cloud := countingCloud(t, "cloud answer", http.StatusOK, &calls)
	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	h.cloud = anthropicAt(cloud.URL)
	h.metrics = &metrics.Counters{}
	a := &fakeAuditLogger{}
	h.audit = a

	res := h.summarizeOne(testAlert())

	if res.AnalyzedBy != "cloud" {
		t.Fatalf("res = %+v, want escalated", res)
	}
	if got := auditActions(a); len(got) != 1 || got[0] != "escalation.trigger" {
		t.Fatalf("audit actions = %v, want exactly [escalation.trigger]", got)
	}
	e := a.entries[0]
	if e.Actor != "system" || e.Target != "alertname=cpu_high host=test-host" {
		t.Errorf("entry = %+v, want actor=system and the alert as target", e)
	}
	wantContains(t, "detail", e.Detail, "local model requested escalation: unsure", `target="anthropic"`, "result=ok")
}

func TestEscalationAudit_FailedEscalationIsRecordedWithError(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary":"local","confidence":"low","escalate":true,"reason":"unsure"}`)
	defer llmSrv.Close()
	var calls int
	cloud := countingCloud(t, "", http.StatusInternalServerError, &calls)
	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	h.cloud = anthropicAt(cloud.URL)
	h.metrics = &metrics.Counters{}
	a := &fakeAuditLogger{}
	h.audit = a

	res := h.summarizeOne(testAlert())

	if res.AnalyzedBy != "local" {
		t.Fatalf("res = %+v, want the local result after a failed escalation", res)
	}
	if got := auditActions(a); len(got) != 1 || got[0] != "escalation.trigger" {
		t.Fatalf("audit actions = %v, want [escalation.trigger]", got)
	}
	wantContains(t, "detail", a.entries[0].Detail, "result=failed", "error=")
}

func TestEscalationAudit_RateLimitedIsRecorded(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	down := newDownLLM(t)
	var calls int
	cloud := countingCloud(t, "x", http.StatusOK, &calls)
	h := newTestHandler(t, lokiSrv.URL, down.URL)
	h.cloud = anthropicAt(cloud.URL)
	h.escalation = config.EscalationConfig{MaxPerHour: 1}
	h.metrics = &metrics.Counters{}
	a := &fakeAuditLogger{}
	h.audit = a

	h.summarizeOne(testAlert()) // takes the only slot
	h.summarizeOne(testAlert()) // blocked by the guardrail

	got := strings.Join(auditActions(a), ",")
	if got != "escalation.trigger,escalation.rate_limited" {
		t.Fatalf("audit actions = %s, want escalation.trigger then escalation.rate_limited", got)
	}
	wantContains(t, "rate-limited detail", a.entries[1].Detail, "max_per_hour=1", "local LLM unavailable")
}

func TestEscalationAudit_NothingRecordedWithoutEscalation(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary":"local","confidence":"high","escalate":false,"reason":"x"}`)
	defer llmSrv.Close()
	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	h.metrics = &metrics.Counters{}
	a := &fakeAuditLogger{}
	h.audit = a

	h.summarizeOne(testAlert())

	if len(a.entries) != 0 {
		t.Errorf("audit entries = %+v, want none for an alert that never escalated", a.entries)
	}
}
