// Package suppress turns confirmed-incident history into candidate
// Alertmanager suppression rules — never applies them.
//
// The idea: if the same (alertname, host) has been confirmed as "known,
// not actionable" over and over, that's a signal the noise should be
// silenced at the source instead of re-analyzed (an LLM call, possibly a
// cloud escalation, possibly a filed issue) every single time it fires.
// This package only finds and describes that pattern; a human decides
// whether to actually apply the resulting rule. See the "victoria-gateway
// suppression-candidates" CLI command for how a candidate reaches a human.
//
// What this can't tell you: the confirmed-record count below is not a
// count of every time the alert fired. It's a count of every time it fired
// *and got analyzed and later confirmed* — an alert that keeps firing but
// whose Pending records never got a resolution written won't show up as
// high-count here, and one that stopped firing but still has old confirmed
// rows will look busier than it currently is. Treat Count as "how often
// this shape was confirmed as noise," not as a live firing rate.
package suppress

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

// Candidate is one (AlertName, Host) pair that has been confirmed often
// enough to be worth considering for a suppression rule.
type Candidate struct {
	AlertName string
	Host      string
	// Count is how many Confirmed records match this AlertName+Host —
	// see the package doc for what this does and doesn't measure.
	Count int
	// FirstConfirmed/LastConfirmed bound the observed window this Count
	// was computed over.
	FirstConfirmed time.Time
	LastConfirmed  time.Time
	// SampleResolution is the most recent confirmed resolution for this
	// pair — shown so a human reviewing the candidate can see *what* kept
	// getting confirmed as the same thing, not just that it did.
	SampleResolution string
}

// FindCandidates groups confirmed into (AlertName, Host) buckets and
// returns every bucket whose Count is at least minCount, most-confirmed
// first. minCount <= 0 defaults to 3 — one or two confirmations of the
// same thing isn't a pattern yet, it's coincidence; three is a
// deliberately low bar (see the CLI's --min-count flag to raise it), on
// the theory that a false candidate here costs a human thirty seconds of
// reading a proposal, while a missed one costs nothing since nothing is
// applied automatically either way.
func FindCandidates(confirmed []rag.Record, minCount int) []Candidate {
	if minCount <= 0 {
		minCount = 3
	}

	type key struct{ alertName, host string }
	buckets := make(map[key]*Candidate)
	var order []key // preserves first-seen order for a stable tie-break in the final sort

	for _, r := range confirmed {
		k := key{r.AlertName, r.Host}
		c, ok := buckets[k]
		if !ok {
			c = &Candidate{AlertName: r.AlertName, Host: r.Host}
			buckets[k] = c
			order = append(order, k)
		}
		c.Count++
		if c.FirstConfirmed.IsZero() || (!r.ConfirmedAt.IsZero() && r.ConfirmedAt.Before(c.FirstConfirmed)) {
			c.FirstConfirmed = r.ConfirmedAt
		}
		if r.ConfirmedAt.After(c.LastConfirmed) {
			c.LastConfirmed = r.ConfirmedAt
			c.SampleResolution = r.Resolution // records are fed in ConfirmedAt-ascending order by AllConfirmed, but this also holds if that ever changes
		}
	}

	candidates := make([]Candidate, 0, len(order))
	for _, k := range order {
		c := buckets[k]
		if c.Count >= minCount {
			candidates = append(candidates, *c)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Count != candidates[j].Count {
			return candidates[i].Count > candidates[j].Count
		}
		// Stable, deterministic tie-break so output order doesn't depend
		// on Go's unordered map iteration.
		if candidates[i].AlertName != candidates[j].AlertName {
			return candidates[i].AlertName < candidates[j].AlertName
		}
		return candidates[i].Host < candidates[j].Host
	})
	return candidates
}

// yamlQuote wraps s in double quotes, escaping the characters YAML's
// double-quoted scalar form requires — sufficient for the alert names and
// hostnames this package handles (Alertmanager label values), not a
// general-purpose YAML encoder.
func yamlQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// RouteYAML renders c as an Alertmanager `route` entry that a human can
// paste under their config's top-level `route.routes:` list, matching
// this exact (alertname, host) to a receiver that does nothing (commonly
// named "null" or "silence" — Alertmanager has no built-in no-op
// receiver, so one has to exist in `receivers:` with no integrations
// configured). `continue: false` (the default, shown explicitly here) is
// what actually stops the notification — without it, matching alerts
// would fall through to whatever route comes after. This is Alertmanager's
// standard way to permanently stop being notified about a specific known
// alert, and unlike a silence it doesn't expire — which is exactly why it
// belongs behind a human decision rather than something this package
// applies on its own: a permanent rule is much easier to add than to
// remember to remove later if the alert's cause comes back.
func (c Candidate) RouteYAML(nullReceiverName string) string {
	if nullReceiverName == "" {
		nullReceiverName = "null"
	}
	return fmt.Sprintf(`- matchers:
    - alertname=%s
    - host=%s
  receiver: %s
  continue: false`,
		yamlQuote(c.AlertName), yamlQuote(c.Host), nullReceiverName)
}

// SilenceCLI renders the amtool command to create a time-bounded silence
// for this candidate instead of a permanent routing change — the more
// conservative option when a human wants to stop the noise now and
// revisit later rather than commit to a permanent rule. durationFlag is
// passed through to amtool verbatim (e.g. "720h" for 30 days); callers
// should validate it's a duration amtool accepts before using this in a
// script.
func (c Candidate) SilenceCLI(durationFlag, comment string) string {
	return fmt.Sprintf(`amtool silence add alertname=%s host=%s --duration=%s --comment=%s`,
		c.AlertName, c.Host, durationFlag, yamlQuote(comment))
}
