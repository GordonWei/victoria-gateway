// Package notify is victoria-gateway's notification layer: it takes one
// analyzed alert's outcome and delivers it to the right place(s). Before
// this package, delivery was a single hard-coded Telegram push inside the
// webhook handler; pulling it out gives three things the design doc
// (_draft_victoria_gateway_next_features.md §3) asked for — multiple
// channels, label-based routing, and a stable interface the
// similar-incident rendering can attach to — without the handler knowing
// any channel's details.
//
// Delivery stays best-effort, matching the old behavior: a failed push is
// logged and counted, never surfaced back to Alertmanager (which would
// only cause a pointless redelivery of an alert that was already
// analyzed successfully).
package notify

import (
	"fmt"
	"log"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/maintenance"
)

// Message is one analyzed alert's outcome, in channel-neutral form. Each
// Channel renders it in its own format (Telegram HTML, webhook JSON).
// Exactly one of Summary/Error is meaningful, mirroring the webhook
// response's alertResult contract.
type Message struct {
	AlertName  string
	Host       string
	Summary    string
	AnalyzedBy string // "local" or "cloud" (empty when Error is set)
	Error      string

	// Similar lists past confirmed incidents worth showing next to this
	// alert (already filtered by similarity threshold by the caller).
	// Channels render it as a "相似歷史事件" section; empty means render
	// nothing extra.
	Similar []SimilarIncident

	// PendingURL, if non-empty, links directly to this alert's own
	// /pending/{id} confirm page — the one place a human reading this
	// notification can actually act on it, as opposed to Similar's links
	// to unrelated past incidents. Empty when RAG/capture is off, capture
	// failed, or no public base URL is configured to build an absolute
	// link from.
	PendingURL string
}

// SimilarIncident is one past confirmed incident reference attached to a
// Message — just enough to render a line a human can follow to the full
// context, not the record itself.
type SimilarIncident struct {
	Ref  string // e.g. "#12 NodeExporterDown (172.16.100.6)" or "incident/8 — InstanceDown (172.16.100.7)"
	Date string // capture date, YYYY-MM-DD
	URL  string // issue URL or /incidents/{id} URL; may be a bare path if no public base URL is configured
}

// Channel delivers a Message somewhere. Implementations own their
// transport, formatting, and retry policy; Send returns an error only
// after those are exhausted.
type Channel interface {
	Name() string
	Send(msg Message) error
}

// Route sends matching alerts to a set of channels. Matchers use the
// same label-glob syntax as maintenance windows (see
// maintenance.MatchLabels). A Default route matches everything and acts
// as the fallback for alerts no earlier route claimed.
type Route struct {
	Matchers map[string]string
	Channels []string
	Default  bool
}

// Router picks the first matching Route for an alert's labels and
// dispatches the Message to that route's channels. First-match, flat
// list, no nesting — deliberately simpler than Alertmanager's routing
// tree; see the design doc.
type Router struct {
	channels map[string]Channel
	routes   []Route

	// onResult is called after each channel delivery attempt (nil ok) —
	// the metrics hook, kept as a callback so this package doesn't import
	// pkg/metrics.
	onResult func(channel string, err error)
}

// NewRouter validates that every route references only defined channels
// and builds the Router. A Router with no channels is valid and inert —
// Dispatch becomes a no-op — matching the old "no bot token, no push"
// behavior.
func NewRouter(channels []Channel, routes []Route, onResult func(channel string, err error)) (*Router, error) {
	byName := make(map[string]Channel, len(channels))
	for _, c := range channels {
		if _, dup := byName[c.Name()]; dup {
			return nil, fmt.Errorf("notify: duplicate channel name %q", c.Name())
		}
		byName[c.Name()] = c
	}
	for i, r := range routes {
		if len(r.Channels) == 0 {
			return nil, fmt.Errorf("notify: route %d has no channels", i)
		}
		for _, name := range r.Channels {
			if _, ok := byName[name]; !ok {
				return nil, fmt.Errorf("notify: route %d references undefined channel %q", i, name)
			}
		}
	}
	return &Router{channels: byName, routes: routes, onResult: onResult}, nil
}

// Enabled reports whether dispatching can deliver anywhere at all.
// Nil-safe, like the handler's other optional collaborators.
func (r *Router) Enabled() bool {
	return r != nil && len(r.channels) > 0 && len(r.routes) > 0
}

// Dispatch delivers msg to every channel of the first route whose
// matchers all match labels (a Default route matches anything). No
// matching route means the alert is intentionally unrouted — logged at
// debug-ish level and dropped, since an operator who writes routes
// without a default has said "everything else, stay quiet".
//
// Channels are attempted sequentially and independently: one failing
// channel never stops the others, and every attempt reports through
// onResult so per-channel metrics see both outcomes.
func (r *Router) Dispatch(msg Message, labels map[string]string) {
	if !r.Enabled() {
		return
	}
	route := r.match(labels)
	if route == nil {
		log.Printf("notify: alert %q matched no route (and no default route is configured), not delivered", msg.AlertName)
		return
	}
	for _, name := range route.Channels {
		ch := r.channels[name]
		err := ch.Send(msg)
		if err != nil {
			log.Printf("notify: channel %q push failed for alert %q: %v", name, msg.AlertName, err)
		}
		if r.onResult != nil {
			r.onResult(name, err)
		}
	}
}

func (r *Router) match(labels map[string]string) *Route {
	for i := range r.routes {
		rt := &r.routes[i]
		if rt.Default {
			return rt
		}
		if maintenance.MatchLabels(rt.Matchers, labels) {
			return rt
		}
	}
	return nil
}

// withRetry runs attempt up to attempts times, sleeping backoff[i]
// between tries, but only re-trying when attempt reported the failure as
// retryable (transport error, 5xx, 429) — a 400-class rejection like
// malformed HTML will fail identically every time, and retrying it just
// delays the log line that tells the operator what's wrong. sleep is
// injectable so tests don't wait out real backoffs.
func withRetry(attempts int, backoff []time.Duration, sleep func(time.Duration), attempt func() (retryable bool, err error)) error {
	if sleep == nil {
		sleep = time.Sleep
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		retryable, err := attempt()
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable || i == attempts-1 {
			return err
		}
		d := backoff[len(backoff)-1]
		if i < len(backoff) {
			d = backoff[i]
		}
		sleep(d)
	}
	return lastErr
}
