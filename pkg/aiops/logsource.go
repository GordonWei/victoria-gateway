package aiops

import (
	"context"
	"fmt"
	"time"
)

// LogSource fetches recent logs for whatever an alert is about, in
// whatever native query shape its backend needs. Loki (the original,
// still-default backend) implements this via lokiLogSource below;
// pkg/cloudwatch and pkg/gcplogging are alternative backends selected by
// config.LogSourceConfig.Type and wired in main.go — the rest of the
// codebase (summarizeOne) calls whichever one is configured through this
// interface and never imports either concrete package directly, the same
// pattern pkg/tracker.Tracker uses for Gitea/GitHub.
type LogSource interface {
	// QueryRange returns log entries for the given identity within
	// [start, end], truncated to at most limit entries (0 = backend
	// default). Which LogIdentity fields actually get used depends on the
	// backend: Loki turns them into a LogQL selector, CloudWatch/GCP build
	// their own native filter instead — see LogIdentity's doc comment.
	QueryRange(ctx context.Context, id LogIdentity, start, end time.Time, limit int) ([]LogEntry, error)
}

// LogIdentity is what Alert.LogIdentity extracts from an alert's labels —
// namespace/pod/deployment/statefulset/host, whichever combination
// applies (see Alert.AffectedIdentity's doc comment for why the precedence
// among them is what it is). A zero-value field means "not present for
// this alert," not "match everything" — callers check Pod/Deployment/
// StatefulSet/Host in that same precedence order to decide which one to
// build a query from.
type LogIdentity struct {
	Namespace   string
	Pod         string
	Deployment  string
	StatefulSet string
	Host        string
}

// LokiSelector renders this identity as a LogQL stream selector, the exact
// strings AffectedIdentity produced before LogSource existed — moving this
// here (rather than leaving it inline in AffectedIdentity) is what let
// CloudWatch/GCP reuse the same precedence logic without parsing LogQL
// back apart to recover the fields that went into it.
func (id LogIdentity) LokiSelector() string {
	if id.Pod != "" {
		return fmt.Sprintf(`{namespace=%q,pod=%q}`, id.Namespace, id.Pod)
	}
	if id.Namespace != "" {
		return fmt.Sprintf(`{namespace=%q}`, id.Namespace)
	}
	return fmt.Sprintf(`{host=%q}`, id.Host)
}

// Term picks the single most specific identifying string out of this
// identity, in the same pod > deployment > statefulset > host precedence
// as everywhere else — for backends like CloudWatch Logs Insights and GCP
// Cloud Logging whose native query language has no concept of Loki's
// multi-label stream selector and instead filters on a single search term
// against the log line/payload.
func (id LogIdentity) Term() string {
	switch {
	case id.Pod != "":
		return id.Pod
	case id.Deployment != "":
		return id.Deployment
	case id.StatefulSet != "":
		return id.StatefulSet
	default:
		return id.Host
	}
}

// lokiLogSource adapts *Client (whose QueryRange takes a pre-built LogQL
// selector string, and is used directly by existing callers/tests that
// predate LogSource) to the LogSource interface without changing Client's
// own method signature.
type lokiLogSource struct{ client *Client }

// NewLokiLogSource wraps an existing Loki Client as a LogSource.
func NewLokiLogSource(client *Client) LogSource {
	return lokiLogSource{client: client}
}

func (l lokiLogSource) QueryRange(_ context.Context, id LogIdentity, start, end time.Time, limit int) ([]LogEntry, error) {
	return l.client.QueryRange(id.LokiSelector(), start, end, limit)
}
