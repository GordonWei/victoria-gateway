// Package metrics is victoria-gateway's own self-observability: a small
// set of counters exposed at GET /metrics in Prometheus text exposition
// format, so an operator can see escalation/capture/failure rates without
// grepping process logs. Hand-rolled rather than pulling in
// prometheus/client_golang — a dozen plain counters don't need a metrics
// framework, and this keeps the dependency list matching the rest of the
// project (stdlib plus exactly what each feature needs).
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Counters holds victoria-gateway's counters. A nil *Counters is valid:
// every Inc method on it is a no-op, so handlers built in tests without
// wiring metrics (most of cmd/victoria-gateway's table-driven tests use a
// bare &handler{}) don't need a nil check at every call site — the same
// "missing wiring degrades to no-op, not a crash" pattern the rest of this
// package's caller already uses (see claimFingerprint's empty-fingerprint
// case in cmd/victoria-gateway/main.go).
type Counters struct {
	alertsTotal                     atomic.Int64
	alertsErrorTotal                atomic.Int64
	dedupSkippedTotal               atomic.Int64
	resolvedSkippedTotal            atomic.Int64
	webhookAuthRejectedTotal        atomic.Int64
	escalationsTotal                atomic.Int64
	escalationFailuresTotal         atomic.Int64
	escalationRateLimitedTotal      atomic.Int64
	ragCaptureTotal                 atomic.Int64
	ragCaptureFailuresTotal         atomic.Int64
	ragSearchFailuresTotal          atomic.Int64
	trackerCreateIssueFailuresTotal atomic.Int64
	telegramPushFailuresTotal       atomic.Int64
	maintenanceSuppressedTotal      atomic.Int64
	maintenanceMutedTotal           atomic.Int64

	// Per-channel notification delivery outcomes, labeled by channel
	// name. A map behind a mutex rather than more atomic fields because
	// channel names are config-defined, not known at compile time.
	notifyPushTotal        labeledCounter
	notifyPushFailureTotal labeledCounter

	// Duration observations (sum + count pairs, enough for Grafana to
	// graph an average) — deliberately not histograms: hand-rolling
	// buckets buys little for a home-lab service, and this keeps the
	// no-client_golang dependency stance.
	analysisDuration  durationStat // whole summarizeOne, per analyzed alert
	lokiQueryDuration durationStat
	localLLMDuration  durationStat
	cloudLLMDuration  durationStat
	ragSearchDuration durationStat // embed + search, the retrieval side only
}

// labeledCounter is a counter family with one string label. Zero value
// is ready to use.
type labeledCounter struct {
	mu   sync.Mutex
	vals map[string]int64
}

func (l *labeledCounter) inc(label string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.vals == nil {
		l.vals = make(map[string]int64)
	}
	l.vals[label]++
}

// snapshot returns the labels in sorted order for stable exposition
// output (scrape diffs and tests both appreciate determinism).
func (l *labeledCounter) snapshot() (labels []string, vals map[string]int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	vals = make(map[string]int64, len(l.vals))
	for k, v := range l.vals {
		vals[k] = v
		labels = append(labels, k)
	}
	sort.Strings(labels)
	return labels, vals
}

// durationStat accumulates observations as a sum (nanoseconds, in an
// atomic int) and a count — the two series Prometheus needs to compute
// an average rate. Zero value is ready to use.
type durationStat struct {
	sumNanos atomic.Int64
	count    atomic.Int64
}

func (d *durationStat) observe(dur time.Duration) {
	d.sumNanos.Add(int64(dur))
	d.count.Add(1)
}

func (c *Counters) IncAlertsTotal() {
	if c != nil {
		c.alertsTotal.Add(1)
	}
}

func (c *Counters) IncAlertsErrorTotal() {
	if c != nil {
		c.alertsErrorTotal.Add(1)
	}
}

func (c *Counters) IncDedupSkippedTotal() {
	if c != nil {
		c.dedupSkippedTotal.Add(1)
	}
}

func (c *Counters) IncResolvedSkippedTotal() {
	if c != nil {
		c.resolvedSkippedTotal.Add(1)
	}
}

func (c *Counters) IncWebhookAuthRejectedTotal() {
	if c != nil {
		c.webhookAuthRejectedTotal.Add(1)
	}
}

func (c *Counters) IncEscalationsTotal() {
	if c != nil {
		c.escalationsTotal.Add(1)
	}
}

func (c *Counters) IncEscalationFailuresTotal() {
	if c != nil {
		c.escalationFailuresTotal.Add(1)
	}
}

func (c *Counters) IncEscalationRateLimitedTotal() {
	if c != nil {
		c.escalationRateLimitedTotal.Add(1)
	}
}

func (c *Counters) IncRAGCaptureTotal() {
	if c != nil {
		c.ragCaptureTotal.Add(1)
	}
}

func (c *Counters) IncRAGCaptureFailuresTotal() {
	if c != nil {
		c.ragCaptureFailuresTotal.Add(1)
	}
}

func (c *Counters) IncRAGSearchFailuresTotal() {
	if c != nil {
		c.ragSearchFailuresTotal.Add(1)
	}
}

func (c *Counters) IncTrackerCreateIssueFailuresTotal() {
	if c != nil {
		c.trackerCreateIssueFailuresTotal.Add(1)
	}
}

func (c *Counters) IncTelegramPushFailuresTotal() {
	if c != nil {
		c.telegramPushFailuresTotal.Add(1)
	}
}

func (c *Counters) IncMaintenanceSuppressedTotal() {
	if c != nil {
		c.maintenanceSuppressedTotal.Add(1)
	}
}

func (c *Counters) IncMaintenanceMutedTotal() {
	if c != nil {
		c.maintenanceMutedTotal.Add(1)
	}
}

// IncNotifyPush records one notification delivery attempt's outcome for
// the named channel. Failures also bump the attempt counter — total is
// attempts, not successes, so failure/total is a meaningful ratio.
func (c *Counters) IncNotifyPush(channel string, failed bool) {
	if c == nil {
		return
	}
	c.notifyPushTotal.inc(channel)
	if failed {
		c.notifyPushFailureTotal.inc(channel)
	}
}

func (c *Counters) ObserveAnalysisDuration(d time.Duration) {
	if c != nil {
		c.analysisDuration.observe(d)
	}
}

func (c *Counters) ObserveLokiQueryDuration(d time.Duration) {
	if c != nil {
		c.lokiQueryDuration.observe(d)
	}
}

func (c *Counters) ObserveLocalLLMDuration(d time.Duration) {
	if c != nil {
		c.localLLMDuration.observe(d)
	}
}

func (c *Counters) ObserveCloudLLMDuration(d time.Duration) {
	if c != nil {
		c.cloudLLMDuration.observe(d)
	}
}

func (c *Counters) ObserveRAGSearchDuration(d time.Duration) {
	if c != nil {
		c.ragSearchDuration.observe(d)
	}
}

// counterDef pairs a counter's exposition name/help with a snapshot of its
// current value, taken at Handler-call time.
type counterDef struct {
	name string
	help string
	val  int64
}

func (c *Counters) snapshot() []counterDef {
	if c == nil {
		c = &Counters{}
	}
	return []counterDef{
		{"victoria_gateway_alerts_total", "Alerts analyzed (excludes resolved and deduped deliveries).", c.alertsTotal.Load()},
		{"victoria_gateway_alerts_error_total", "Alerts that failed before producing a summary (bad host label, Loki error, LLM error).", c.alertsErrorTotal.Load()},
		{"victoria_gateway_dedup_skipped_total", "Deliveries skipped as a duplicate of an already-claimed alert fingerprint.", c.dedupSkippedTotal.Load()},
		{"victoria_gateway_resolved_skipped_total", "Resolved deliveries received (never analyzed).", c.resolvedSkippedTotal.Load()},
		{"victoria_gateway_webhook_auth_rejected_total", "Webhook requests rejected for missing or wrong Basic Auth credentials.", c.webhookAuthRejectedTotal.Load()},
		{"victoria_gateway_escalations_total", "Alerts successfully escalated to and answered by the cloud model.", c.escalationsTotal.Load()},
		{"victoria_gateway_escalation_failures_total", "Escalation attempts where the cloud call itself failed (fell back to the local result).", c.escalationFailuresTotal.Load()},
		{"victoria_gateway_escalation_rate_limited_total", "Escalations skipped because escalation.max_per_hour was already reached (stayed on the local result).", c.escalationRateLimitedTotal.Load()},
		{"victoria_gateway_rag_capture_total", "Incidents successfully captured as a Pending RAG record.", c.ragCaptureTotal.Load()},
		{"victoria_gateway_rag_capture_failures_total", "RAG capture attempts that failed (embedding or Postgres insert error).", c.ragCaptureFailuresTotal.Load()},
		{"victoria_gateway_rag_search_failures_total", "RAG past-incident lookups that failed (embedding or Postgres search error).", c.ragSearchFailuresTotal.Load()},
		{"victoria_gateway_tracker_create_issue_failures_total", "Gitea/GitHub issue creation failures during capture.", c.trackerCreateIssueFailuresTotal.Load()},
		{"victoria_gateway_telegram_push_failures_total", "Telegram summary push failures.", c.telegramPushFailuresTotal.Load()},
		{"victoria_gateway_maintenance_suppressed_total", "Alerts suppressed (skipped entirely) due to an active maintenance window.", c.maintenanceSuppressedTotal.Load()},
		{"victoria_gateway_maintenance_muted_total", "Alerts muted (analyzed but not pushed) due to an active maintenance window.", c.maintenanceMutedTotal.Load()},
	}
}

// durationDef pairs one durationStat with its exposition base name.
type durationDef struct {
	name string
	help string
	stat *durationStat
}

func (c *Counters) durations() []durationDef {
	return []durationDef{
		{"victoria_gateway_analysis_duration_seconds", "Whole per-alert analysis time (Loki + LLM + escalation + RAG capture).", &c.analysisDuration},
		{"victoria_gateway_loki_query_duration_seconds", "Loki query_range call time.", &c.lokiQueryDuration},
		{"victoria_gateway_local_llm_duration_seconds", "Local summarizer LLM call time.", &c.localLLMDuration},
		{"victoria_gateway_cloud_llm_duration_seconds", "Cloud escalation call time (successful or not).", &c.cloudLLMDuration},
		{"victoria_gateway_rag_search_duration_seconds", "RAG retrieval time (embed + vector search).", &c.ragSearchDuration},
	}
}

// Handler serves the counters at GET /metrics in Prometheus text
// exposition format. Safe to call on a nil *Counters (renders every
// counter as 0), matching the Inc methods' nil-safety above.
func (c *Counters) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		for _, d := range c.snapshot() {
			// A write failure here just means the client went away
			// mid-scrape; nothing useful to do about it, and the next
			// scrape interval will just try again.
			_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", d.name, d.help, d.name, d.name, d.val)
		}
		if c == nil {
			return
		}
		for _, family := range []struct {
			name string
			lc   *labeledCounter
		}{
			{"victoria_gateway_notify_push_total", &c.notifyPushTotal},
			{"victoria_gateway_notify_push_failure_total", &c.notifyPushFailureTotal},
		} {
			name, lc := family.name, family.lc
			labels, vals := lc.snapshot()
			if len(labels) == 0 {
				continue
			}
			_, _ = fmt.Fprintf(w, "# TYPE %s counter\n", name)
			for _, label := range labels {
				_, _ = fmt.Fprintf(w, "%s{channel=%q} %d\n", name, label, vals[label])
			}
		}
		for _, d := range c.durations() {
			sum := time.Duration(d.stat.sumNanos.Load()).Seconds()
			count := d.stat.count.Load()
			_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s_sum counter\n%s_sum %.6f\n# TYPE %s_count counter\n%s_count %d\n",
				d.name, d.help, d.name, d.name, sum, d.name, d.name, count)
		}
	})
}
