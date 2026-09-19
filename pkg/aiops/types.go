// Package aiops implements the AIOps hub: Alertmanager webhook -> Loki log
// fetch -> local LLM summary. See Cowork/docs/_draft_orch_aiops_hub_1031_plan.md
// for the overall design and docs/_agent_handoff.md for task assignment.
//
// This file defines the shared contract types used across webhook.go,
// loki.go, and summarize.go. Defining them here (rather than letting each
// file declare its own copy) keeps the three pieces — split across two
// contributors — compiling against one definition instead of drifting.
package aiops

import (
	"time"
)

// WebhookPayload mirrors the JSON body Alertmanager's webhook receiver POSTs.
// Field set and shapes are the official format documented at
// https://prometheus.io/docs/alerting/latest/configuration/#webhook_config —
// verified against that page on 2026-08-20, not guessed.
type WebhookPayload struct {
	Version            string            `json:"version"`
	GroupKey           string            `json:"groupKey"`
	TruncatedAlerts    int               `json:"truncatedAlerts"`
	Status             string            `json:"status"` // "resolved" | "firing"
	Receiver           string            `json:"receiver"`
	GroupLabels        map[string]string `json:"groupLabels"`
	CommonLabels       map[string]string `json:"commonLabels"`
	CommonAnnotations  map[string]string `json:"commonAnnotations"`
	ExternalURL        string            `json:"externalURL"`
	NotificationReason string            `json:"notification_reason"`
	Alerts             []Alert           `json:"alerts"`
}

// Alert is one entry in WebhookPayload.Alerts.
type Alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"` // RFC3339
	EndsAt       string            `json:"endsAt"`   // RFC3339; "0001-01-01T00:00:00Z" while firing
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

// Host returns the label this alert uses to identify the affected machine.
// Alertmanager rule authors aren't consistent about naming this label
// "instance" vs "host" — this checks both rather than assuming one.
// Returns ok=false if neither is set, so callers can fail loudly instead of
// querying Loki with an empty host filter.
func (a Alert) Host() (host string, ok bool) {
	if v, present := a.Labels["host"]; present && v != "" {
		return v, true
	}
	if v, present := a.Labels["instance"]; present && v != "" {
		return v, true
	}
	return "", false
}

// AffectedIdentity returns what this alert is about, in the two shapes
// callers need: a human-readable display string (used for res.Host, RAG's
// Record.Host, Gitea issue titles, and the LLM prompt's "主機：" line) and
// the LogQL stream selector that actually finds this alert's logs in Loki.
//
// Two label vocabularies exist in this environment and they don't share a
// dimension. node_exporter/domain-exporter/blackbox alerts carry
// host/instance (an IP:port) and match syslog/journald logs, which Loki
// labels by machine `host`. kube-state-metrics-sourced alerts carry
// `namespace` instead — their `instance` label is kube-state-metrics' own
// scrape address, not the affected workload's host, and using it against
// Loki finds nothing. Pod logs are shipped by the in-cluster Alloy
// DaemonSet (loki.source.kubernetes.pods) and labeled by
// `namespace`/`pod`/`container`, a completely different vocabulary.
// Matching a K8s alert's instance against Loki's host label doesn't
// error — it's a well-formed query that always returns zero rows, which
// is exactly the silent-failure shape this method exists to close.
//
// kube-state-metrics alerts split into two shapes that need different
// selectors:
//   - Pod-level (KubePodCrashLooping, KubePodNotReady) carry `namespace`+
//     `pod` — the most specific selector available, preferred whenever
//     both are present.
//   - Workload-level (KubeDeploymentReplicasMismatch,
//     KubeStatefulSetReplicasMismatch — see homelab-infra
//     clusters/home/kube-state-metrics/) describe a Deployment or
//     StatefulSet object, which kube-state-metrics never attaches a `pod`
//     label to (there's no single pod the alert is "about"). Falling back
//     straight to host/instance here would hit the exact meaningless
//     scrape-address problem this method exists to avoid. A
//     namespace-only selector is broader than a pod-scoped one — it
//     returns every pod's logs in that namespace, not just the troubled
//     workload's — but broader-and-real beats narrower-and-empty, so it's
//     used whenever a `deployment` or `statefulset` label confirms this is
//     actually a workload-level alert (not just an alert that happens to
//     carry a stray `namespace` label — see
//     TestAffectedIdentity_PartialNamespacePodFallsBackToHost).
//
// host/instance is the last-resort fallback for traditional infrastructure
// alerts that carry neither shape.
func (a Alert) AffectedIdentity() (display, lokiSelector string, ok bool) {
	display, id, ok := a.identity()
	if !ok {
		return "", "", false
	}
	return display, id.LokiSelector(), true
}

// LogIdentity is the structured form of the same precedence AffectedIdentity
// applies, for LogSource implementations whose native query language isn't
// LogQL (CloudWatch Logs Insights, GCP Cloud Logging filters) — they build
// their own query from whichever fields are non-empty instead of parsing a
// pre-built LogQL selector string back apart. Exactly the same fields
// AffectedIdentity's doc comment explains the precedence of; see it for why
// pod beats deployment/statefulset beats host.
func (a Alert) LogIdentity() (display string, id LogIdentity, ok bool) {
	return a.identity()
}

// identity is the shared precedence logic behind AffectedIdentity and
// LogIdentity — see AffectedIdentity's doc comment for why this order
// (pod > deployment > statefulset > host) exists. Kept as one
// implementation so the two public methods can't silently drift apart.
func (a Alert) identity() (display string, id LogIdentity, ok bool) {
	ns := a.Labels["namespace"]
	if pod := a.Labels["pod"]; ns != "" && pod != "" {
		return ns + "/" + pod, LogIdentity{Namespace: ns, Pod: pod}, true
	}
	if workload := a.Labels["deployment"]; ns != "" && workload != "" {
		return ns + "/" + workload, LogIdentity{Namespace: ns, Deployment: workload}, true
	}
	if workload := a.Labels["statefulset"]; ns != "" && workload != "" {
		return ns + "/" + workload, LogIdentity{Namespace: ns, StatefulSet: workload}, true
	}
	if h, hostOK := a.Host(); hostOK {
		return h, LogIdentity{Host: h}, true
	}
	return "", LogIdentity{}, false
}

// StartTime parses StartsAt as RFC3339. Returns the zero Time and an error
// if StartsAt is empty or malformed — callers should treat that as fatal
// for building a Loki query window, not silently default to time.Now().
func (a Alert) StartTime() (time.Time, error) {
	return time.Parse(time.RFC3339, a.StartsAt)
}

// LogEntry is one line returned from a Loki query_range call, decoded from
// Loki's `data.result[].values` shape: [["<unix_nanos_string>", "<line>"], ...].
type LogEntry struct {
	Timestamp time.Time
	Line      string
}
