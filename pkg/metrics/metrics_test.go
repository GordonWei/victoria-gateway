package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCounters_NilSafe(t *testing.T) {
	var c *Counters
	// None of these should panic on a nil receiver.
	c.IncAlertsTotal()
	c.IncAlertsErrorTotal()
	c.IncDedupSkippedTotal()
	c.IncResolvedSkippedTotal()
	c.IncWebhookAuthRejectedTotal()
	c.IncEscalationsTotal()
	c.IncEscalationFailuresTotal()
	c.IncEscalationRateLimitedTotal()
	c.IncRAGCaptureTotal()
	c.IncRAGCaptureFailuresTotal()
	c.IncRAGSearchFailuresTotal()
	c.IncTrackerCreateIssueFailuresTotal()
	c.IncTelegramPushFailuresTotal()

	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("nil Counters Handler status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "victoria_gateway_alerts_total 0") {
		t.Errorf("nil Counters should render every counter as 0, got:\n%s", rec.Body.String())
	}
}

func TestCounters_HandlerReflectsIncrements(t *testing.T) {
	c := &Counters{}
	c.IncAlertsTotal()
	c.IncAlertsTotal()
	c.IncAlertsErrorTotal()
	c.IncEscalationsTotal()

	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	for _, want := range []string{
		"victoria_gateway_alerts_total 2",
		"victoria_gateway_alerts_error_total 1",
		"victoria_gateway_escalations_total 1",
		"victoria_gateway_escalation_failures_total 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q, got:\n%s", want, body)
		}
	}
}

func TestCounters_HandlerContentType(t *testing.T) {
	c := &Counters{}
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain prefix", ct)
	}
}
