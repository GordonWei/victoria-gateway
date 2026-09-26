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
	"strings"
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
	// Labeled by the route an alert took ("legacy" without hybrid_routes)
	// or the escalation target involved (the provider name in legacy
	// mode). The metric names predate the labels; sum() over a family
	// gives exactly the number the unlabeled series used to.
	alertsTotal                labeledCounter // route
	alertsErrorTotal           labeledCounter // route
	escalationsTotal           labeledCounter // target
	escalationFailuresTotal    labeledCounter // target
	escalationRateLimitedTotal labeledCounter // target
	escalationNoTargetTotal    labeledCounter // route (there is no target to name)
	logQueryRefusedTotal       labeledCounter // log_source

	dedupSkippedTotal               atomic.Int64
	resolvedSkippedTotal            atomic.Int64
	webhookAuthRejectedTotal        atomic.Int64
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
	analysisDuration  durationStat    // whole summarizeOne, per analyzed alert
	lokiQueryDuration labeledDuration // log_source; covers every backend, not just Loki
	localLLMDuration  durationStat
	cloudLLMDuration  labeledDuration // target
	ragSearchDuration durationStat    // embed + search, the retrieval side only
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

// labeledDuration is a durationStat family with one string label, the
// duration counterpart of labeledCounter. Zero value is ready to use.
type labeledDuration struct {
	mu   sync.Mutex
	vals map[string]*durationStat
}

func (l *labeledDuration) observe(label string, dur time.Duration) {
	l.mu.Lock()
	d, ok := l.vals[label]
	if !ok {
		if l.vals == nil {
			l.vals = make(map[string]*durationStat)
		}
		d = &durationStat{}
		l.vals[label] = d
	}
	l.mu.Unlock()
	d.observe(dur)
}

// snapshot returns sorted labels with each one's (sum seconds, count).
func (l *labeledDuration) snapshot() (labels []string, sums map[string]float64, counts map[string]int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	sums = make(map[string]float64, len(l.vals))
	counts = make(map[string]int64, len(l.vals))
	for k, d := range l.vals {
		labels = append(labels, k)
		sums[k] = time.Duration(d.sumNanos.Load()).Seconds()
		counts[k] = d.count.Load()
	}
	sort.Strings(labels)
	return labels, sums, counts
}

func (c *Counters) IncAlertsTotal(route string) {
	if c != nil {
		c.alertsTotal.inc(route)
	}
}

func (c *Counters) IncAlertsErrorTotal(route string) {
	if c != nil {
		c.alertsErrorTotal.inc(route)
	}
}

// IncLogQueryRefusedTotal counts alerts whose log query was never sent
// because the search term was refused as unsafe (see
// aiops.ErrUnsafeSearchTerm); the alert is still summarized without logs.
func (c *Counters) IncLogQueryRefusedTotal(logSource string) {
	if c != nil {
		c.logQueryRefusedTotal.inc(logSource)
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

func (c *Counters) IncEscalationsTotal(target string) {
	if c != nil {
		c.escalationsTotal.inc(target)
	}
}

func (c *Counters) IncEscalationFailuresTotal(target string) {
	if c != nil {
		c.escalationFailuresTotal.inc(target)
	}
}

// IncEscalationNoTargetTotal counts alerts that asked to escalate but whose
// route had nowhere to escalate to — a hybrid route with no escalation, or
// a legacy config with no cloud block.
func (c *Counters) IncEscalationNoTargetTotal(route string) {
	if c != nil {
		c.escalationNoTargetTotal.inc(route)
	}
}

func (c *Counters) IncEscalationRateLimitedTotal(target string) {
	if c != nil {
		c.escalationRateLimitedTotal.inc(target)
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

// ObserveLokiQueryDuration records one log query, whichever backend
// (Loki, CloudWatch, Cloud Logging) served it — the name is historical.
func (c *Counters) ObserveLokiQueryDuration(logSource string, d time.Duration) {
	if c != nil {
		c.lokiQueryDuration.observe(logSource, d)
	}
}

func (c *Counters) ObserveLocalLLMDuration(d time.Duration) {
	if c != nil {
		c.localLLMDuration.observe(d)
	}
}

func (c *Counters) ObserveCloudLLMDuration(target string, d time.Duration) {
	if c != nil {
		c.cloudLLMDuration.observe(target, d)
	}
}

func (c *Counters) ObserveRAGSearchDuration(d time.Duration) {
	if c != nil {
		c.ragSearchDuration.observe(d)
	}
}

// labelValueEscaper applies the Prometheus text exposition format's
// label-value escaping: backslash, double quote and line feed, nothing
// else. Go's %q is not a substitute — it also turns a tab into \t and
// other non-printable characters into \x../\u.... sequences, which the
// format doesn't define, so a scraper would read back a different value
// (or reject the line) instead of the label that was recorded.
var labelValueEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabelValue(v string) string { return labelValueEscaper.Replace(v) }

// counterDef pairs a counter's exposition name/help with a snapshot of its
// current value, taken at Handler-call time. A labeled family sets label
// and lc instead of val.
type counterDef struct {
	name  string
	help  string
	val   int64
	label string
	lc    *labeledCounter
}

func (c *Counters) snapshot() []counterDef {
	if c == nil {
		c = &Counters{}
	}
	return []counterDef{
		{name: "victoria_gateway_alerts_total", help: "Alerts analyzed (excludes resolved and deduped deliveries).", label: "route", lc: &c.alertsTotal},
		{name: "victoria_gateway_alerts_error_total", help: "Alerts that failed before producing a summary (bad host label, Loki error, LLM error).", label: "route", lc: &c.alertsErrorTotal},
		{name: "victoria_gateway_log_query_refused_total", help: "Log queries skipped because the alert's search term was refused as unsafe (the alert was summarized without logs).", label: "log_source", lc: &c.logQueryRefusedTotal},
		{name: "victoria_gateway_dedup_skipped_total", help: "Deliveries skipped as a duplicate of an already-claimed alert fingerprint.", val: c.dedupSkippedTotal.Load()},
		{name: "victoria_gateway_resolved_skipped_total", help: "Resolved deliveries received (never analyzed).", val: c.resolvedSkippedTotal.Load()},
		{name: "victoria_gateway_webhook_auth_rejected_total", help: "Webhook requests rejected for missing or wrong Basic Auth credentials.", val: c.webhookAuthRejectedTotal.Load()},
		{name: "victoria_gateway_escalations_total", help: "Alerts successfully escalated to and answered by the cloud model.", label: "target", lc: &c.escalationsTotal},
		{name: "victoria_gateway_escalation_failures_total", help: "Escalation attempts where the cloud call itself failed (fell back to the next target, or the local result).", label: "target", lc: &c.escalationFailuresTotal},
		{name: "victoria_gateway_escalation_rate_limited_total", help: "Escalations skipped because escalation.max_per_hour was already reached (stayed on the local result).", label: "target", lc: &c.escalationRateLimitedTotal},
		{name: "victoria_gateway_escalation_no_target_total", help: "Alerts that would have escalated but whose route has no escalation target (stayed on the local result).", label: "route", lc: &c.escalationNoTargetTotal},
		{name: "victoria_gateway_rag_capture_total", help: "Incidents successfully captured as a Pending RAG record.", val: c.ragCaptureTotal.Load()},
		{name: "victoria_gateway_rag_capture_failures_total", help: "RAG capture attempts that failed (embedding or Postgres insert error).", val: c.ragCaptureFailuresTotal.Load()},
		{name: "victoria_gateway_rag_search_failures_total", help: "RAG past-incident lookups that failed (embedding or Postgres search error).", val: c.ragSearchFailuresTotal.Load()},
		{name: "victoria_gateway_tracker_create_issue_failures_total", help: "Gitea/GitHub issue creation failures during capture.", val: c.trackerCreateIssueFailuresTotal.Load()},
		{name: "victoria_gateway_telegram_push_failures_total", help: "Telegram summary push failures.", val: c.telegramPushFailuresTotal.Load()},
		{name: "victoria_gateway_maintenance_suppressed_total", help: "Alerts suppressed (skipped entirely) due to an active maintenance window.", val: c.maintenanceSuppressedTotal.Load()},
		{name: "victoria_gateway_maintenance_muted_total", help: "Alerts muted (analyzed but not pushed) due to an active maintenance window.", val: c.maintenanceMutedTotal.Load()},
	}
}

// durationDef pairs one duration family with its exposition base name.
// Exactly one of stat/ld is set; ld families carry label.
type durationDef struct {
	name  string
	help  string
	stat  *durationStat
	label string
	ld    *labeledDuration
}

func (c *Counters) durations() []durationDef {
	return []durationDef{
		{name: "victoria_gateway_analysis_duration_seconds", help: "Whole per-alert analysis time (Loki + LLM + escalation + RAG capture).", stat: &c.analysisDuration},
		{name: "victoria_gateway_loki_query_duration_seconds", help: "Log query time, whichever backend served it (Loki, CloudWatch, Cloud Logging).", label: "log_source", ld: &c.lokiQueryDuration},
		{name: "victoria_gateway_local_llm_duration_seconds", help: "Local summarizer LLM call time.", stat: &c.localLLMDuration},
		{name: "victoria_gateway_cloud_llm_duration_seconds", help: "Cloud escalation call time (successful or not).", label: "target", ld: &c.cloudLLMDuration},
		{name: "victoria_gateway_rag_search_duration_seconds", help: "RAG retrieval time (embed + vector search).", stat: &c.ragSearchDuration},
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
			_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", d.name, d.help, d.name)
			if d.lc == nil {
				_, _ = fmt.Fprintf(w, "%s %d\n", d.name, d.val)
				continue
			}
			labels, vals := d.lc.snapshot()
			if len(labels) == 0 {
				// Nothing observed yet: one unlabeled 0 keeps the family
				// visible (and the pre-label output unchanged) until the
				// first labeled series appears.
				_, _ = fmt.Fprintf(w, "%s 0\n", d.name)
				continue
			}
			for _, l := range labels {
				_, _ = fmt.Fprintf(w, "%s{%s=\"%s\"} %d\n", d.name, d.label, escapeLabelValue(l), vals[l])
			}
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
				_, _ = fmt.Fprintf(w, "%s{channel=\"%s\"} %d\n", name, escapeLabelValue(label), vals[label])
			}
		}
		for _, d := range c.durations() {
			if d.ld == nil {
				sum := time.Duration(d.stat.sumNanos.Load()).Seconds()
				count := d.stat.count.Load()
				_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s_sum counter\n%s_sum %.6f\n# TYPE %s_count counter\n%s_count %d\n",
					d.name, d.help, d.name, d.name, sum, d.name, d.name, count)
				continue
			}
			labels, sums, counts := d.ld.snapshot()
			_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s_sum counter\n", d.name, d.help, d.name)
			if len(labels) == 0 {
				_, _ = fmt.Fprintf(w, "%s_sum %.6f\n", d.name, 0.0)
			}
			for _, l := range labels {
				_, _ = fmt.Fprintf(w, "%s_sum{%s=\"%s\"} %.6f\n", d.name, d.label, escapeLabelValue(l), sums[l])
			}
			_, _ = fmt.Fprintf(w, "# TYPE %s_count counter\n", d.name)
			if len(labels) == 0 {
				_, _ = fmt.Fprintf(w, "%s_count 0\n", d.name)
			}
			for _, l := range labels {
				_, _ = fmt.Fprintf(w, "%s_count{%s=\"%s\"} %d\n", d.name, d.label, escapeLabelValue(l), counts[l])
			}
		}
	})
}
