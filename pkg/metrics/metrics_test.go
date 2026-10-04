package metrics

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCounters_NilSafe(t *testing.T) {
	var c *Counters
	// None of these should panic on a nil receiver.
	c.IncAlertsTotal("legacy")
	c.IncAlertsErrorTotal("legacy")
	c.IncDedupSkippedTotal()
	c.IncResolvedSkippedTotal()
	c.IncWebhookAuthRejectedTotal()
	c.IncEscalationsTotal("bedrock")
	c.IncEscalationFailuresTotal("bedrock")
	c.IncEscalationRateLimitedTotal("bedrock")
	c.IncEscalationNoTargetTotal("legacy")
	c.ObserveLokiQueryDuration("loki", time.Second)
	c.ObserveCloudLLMDuration("bedrock", time.Second)
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
	c.IncAlertsTotal("legacy")
	c.IncAlertsTotal("legacy")
	c.IncAlertsErrorTotal("legacy")
	c.IncEscalationsTotal("bedrock")

	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	for _, want := range []string{
		`victoria_gateway_alerts_total{route="legacy"} 2`,
		`victoria_gateway_alerts_error_total{route="legacy"} 1`,
		`victoria_gateway_escalations_total{target="bedrock"} 1`,
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

func TestCounters_EscalationNoTarget(t *testing.T) {
	c := &Counters{}
	c.IncEscalationNoTargetTotal("default")
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `victoria_gateway_escalation_no_target_total{route="default"} 1`) {
		t.Errorf("metrics output missing the no-target counter, got:\n%s", rec.Body.String())
	}
}

// TestCounters_LabeledFamilies checks the per-route/target/log-source
// labels: one HELP/TYPE per family, one series per label value, and the
// per-label values summing to what the unlabeled series used to show.
func TestCounters_LabeledFamilies(t *testing.T) {
	c := &Counters{}
	c.IncAlertsTotal("hybrid_routes[0]")
	c.IncAlertsTotal("default")
	c.IncAlertsTotal("default")
	c.IncEscalationsTotal("aws")
	c.IncEscalationFailuresTotal("aws")
	c.IncEscalationFailuresTotal("default")
	c.IncEscalationRateLimitedTotal("aws")
	c.ObserveLokiQueryDuration("onprem", 2*time.Second)
	c.ObserveLokiQueryDuration("aws", time.Second)
	c.ObserveLokiQueryDuration("aws", time.Second)
	c.ObserveCloudLLMDuration("aws", 3*time.Second)

	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	for _, want := range []string{
		`victoria_gateway_alerts_total{route="default"} 2`,
		`victoria_gateway_alerts_total{route="hybrid_routes[0]"} 1`,
		`victoria_gateway_escalations_total{target="aws"} 1`,
		`victoria_gateway_escalation_failures_total{target="aws"} 1`,
		`victoria_gateway_escalation_failures_total{target="default"} 1`,
		`victoria_gateway_escalation_rate_limited_total{target="aws"} 1`,
		"victoria_gateway_escalation_no_target_total 0",
		`victoria_gateway_loki_query_duration_seconds_sum{log_source="aws"} 2.000000`,
		`victoria_gateway_loki_query_duration_seconds_count{log_source="aws"} 2`,
		`victoria_gateway_loki_query_duration_seconds_count{log_source="onprem"} 1`,
		`victoria_gateway_cloud_llm_duration_seconds_count{target="aws"} 1`,
		"victoria_gateway_local_llm_duration_seconds_count 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
	// The unlabeled series must be gone once labeled ones exist, or
	// sum() would double count.
	for _, notWant := range []string{"victoria_gateway_alerts_total 0\n", "victoria_gateway_loki_query_duration_seconds_count 0\n"} {
		if strings.Contains(body, notWant) {
			t.Errorf("metrics output still has unlabeled %q next to labeled series", notWant)
		}
	}
	// Prometheus text format allows one HELP and one TYPE per metric name.
	seen := map[string]int{}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# TYPE ") || strings.HasPrefix(line, "# HELP ") {
			seen[line[:6]+" "+strings.Fields(line)[2]]++
		}
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("%s appears %d times", k, n)
		}
	}
	if got := sumFamily(body, "victoria_gateway_alerts_total"); got != 3 {
		t.Errorf("sum(alerts_total) = %d, want 3", got)
	}
}

// TestCounters_LegacyLabelsSumToOldValue: a legacy deployment only ever
// produces route="legacy" and target=<provider>, so sum() equals the old
// unlabeled number exactly.
func TestCounters_LegacyLabelsSumToOldValue(t *testing.T) {
	c := &Counters{}
	for i := 0; i < 5; i++ {
		c.IncAlertsTotal("legacy")
	}
	c.IncEscalationsTotal("bedrock")
	c.IncEscalationsTotal("bedrock")
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if got := sumFamily(body, "victoria_gateway_alerts_total"); got != 5 {
		t.Errorf("sum(alerts_total) = %d, want 5", got)
	}
	if got := sumFamily(body, "victoria_gateway_escalations_total"); got != 2 {
		t.Errorf("sum(escalations_total) = %d, want 2", got)
	}
}

// sumFamily adds every sample of one counter family (labeled or not).
func sumFamily(body, name string) int64 {
	var total int64
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, name+" ") && !strings.HasPrefix(line, name+"{") {
			continue
		}
		f := strings.Fields(line)
		n, err := strconv.ParseInt(f[len(f)-1], 10, 64)
		if err == nil {
			total += n
		}
	}
	return total
}

func TestEscapeLabelValue(t *testing.T) {
	cases := map[string]string{
		`plain`:       `plain`,
		`a\b`:         `a\\b`,
		`say "hi"`:    `say \"hi\"`,
		"line\nbreak": `line\nbreak`,
		"tab\there":   "tab\there", // not escaped by the format; %q would write \t
		"地端-告警":       "地端-告警",
		"\x01ctl":     "\x01ctl", // %q would write \x01
	}
	for in, want := range cases {
		if got := escapeLabelValue(in); got != want {
			t.Errorf("escapeLabelValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCounters_LabelValuesUsePrometheusEscaping(t *testing.T) {
	c := &Counters{}
	odd := "route \"x\"\\\ttab\nnl"
	wantLabel := `"route \"x\"\\` + "\t" + `tab\nnl"`
	c.IncAlertsTotal(odd)
	c.IncNotifyPush(odd, false)
	c.ObserveCloudLLMDuration(odd, time.Second)

	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`victoria_gateway_alerts_total{route=` + wantLabel + `} 1`,
		`victoria_gateway_notify_push_total{channel=` + wantLabel + `} 1`,
		`victoria_gateway_cloud_llm_duration_seconds_sum{target=` + wantLabel + `} 1.000000`,
		`victoria_gateway_cloud_llm_duration_seconds_count{target=` + wantLabel + `} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q; got:\n%s", want, body)
		}
	}
	if strings.Contains(body, `\t`) {
		t.Error("exposition contains a Go-style \\t escape, which the text format doesn't define")
	}
}

func TestCounters_BuildInfo(t *testing.T) {
	scrapeBody := func(c *Counters) string {
		rec := httptest.NewRecorder()
		c.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		return rec.Body.String()
	}
	c := &Counters{}
	if body := scrapeBody(c); strings.Contains(body, "victoria_gateway_build_info") {
		t.Errorf("build_info emitted before SetBuildInfo:\n%s", body)
	}
	c.SetBuildInfo("v1.14.0", `ab"c1\23`)
	body := scrapeBody(c)
	want := `victoria_gateway_build_info{version="v1.14.0",commit="ab\"c1\\23"} 1`
	if !strings.Contains(body, want) || !strings.Contains(body, "# TYPE victoria_gateway_build_info gauge") {
		t.Errorf("metrics output missing %q, got:\n%s", want, body)
	}
	var nilC *Counters
	nilC.SetBuildInfo("x", "y") // must not panic
	if strings.Contains(scrapeBody(nilC), "build_info") {
		t.Error("nil Counters emitted build_info")
	}
}
