package aiops

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
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

// ForLogQuery returns the identity to search logs with: the same
// identity, except that a Host that is a URL with a scheme and a host —
// a blackbox probe's instance="https://kmp.tw/health" — is cut down to
// the URL's hostname ("kmp.tw", no port, path or query). The URL as a
// whole can't be searched for (SafeTerm refuses its '/', and no Loki
// stream is labeled with it), while the hostname is what the probed
// machine's own logs, if any are collected, would carry.
//
// Anything that doesn't parse as such a URL is returned unchanged,
// including host:port instances like "172.16.100.6:9100" (not a valid
// URL) and "node1:9100" (parses with node1 as the scheme but no host),
// so non-URL alerts query exactly as before. The display string an
// alert is shown and filed under is not affected — only the log query.
func (id LogIdentity) ForLogQuery() LogIdentity {
	id.Host = searchHost(id.Host)
	return id
}

func searchHost(host string) string {
	u, err := url.Parse(host)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Hostname() == "" {
		return host
	}
	return u.Hostname()
}

// SafeTerm returns Term() for splicing into a CloudWatch Logs Insights
// query or a Cloud Logging filter, or an error if the term contains a
// character that could step outside the literal it's placed in. Neither
// query language has a documented escaping rule this code can rely on for
// every operator-written template (the term may land in a regex, a quoted
// string, or bare), so instead of escaping, anything that isn't plausibly
// part of a host/pod/instance name is refused: quotes, backslash, '/',
// '|', backtick, parentheses, whitespace and control characters. A host
// or pod name never contains these, and neither does a probe URL once
// ForLogQuery has reduced it to its hostname. A label that still does —
// an odd hostname, or an injection attempt — can't be searched for
// safely, so that alert's log query is skipped rather than sent.
// The returned error matches ErrUnsafeSearchTerm under errors.Is, so the
// caller can carry on without logs instead of failing the alert.
func (id LogIdentity) SafeTerm() (string, error) {
	term := id.Term()
	for _, r := range term {
		if strings.ContainsRune("\"'\\/|`()", r) || unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", &unsafeTermError{term: term, char: r}
		}
	}
	return term, nil
}

// ErrUnsafeSearchTerm is what SafeTerm's refusal matches under errors.Is.
// A refused term isn't a log backend failure — the query was never sent
// — so the alert is still summarized, just without logs, the same as a
// query that found nothing.
var ErrUnsafeSearchTerm = errors.New("unsafe log search term")

type unsafeTermError struct {
	term string
	char rune
}

func (e *unsafeTermError) Error() string {
	return fmt.Sprintf("refusing to search for %q: it contains %q, which could change the query's meaning", e.term, e.char)
}

func (e *unsafeTermError) Is(target error) bool { return target == ErrUnsafeSearchTerm }

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
