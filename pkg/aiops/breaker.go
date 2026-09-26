package aiops

import (
	"sync"
	"time"
)

// breakerState is where one backend's circuit breaker stands.
type breakerState int

const (
	breakerClosed   breakerState = iota // calls go through
	breakerOpen                         // calls are skipped until the cooldown ends
	breakerHalfOpen                     // one trial call is in flight; everything else is skipped
)

// breakerTransition names a state change for the caller to log and
// report; "" means the state didn't change.
type breakerTransition string

const (
	transitionNone     breakerTransition = ""
	transitionOpen     breakerTransition = "open"
	transitionReopen   breakerTransition = "reopen" // half-open trial failed
	transitionHalfOpen breakerTransition = "half-open"
	transitionClose    breakerTransition = "close"
)

// breaker is a per-backend circuit breaker. After threshold consecutive
// unavailable failures it opens: for cooldown, the backend is skipped
// without a probe or a chat call. The first call after the cooldown is
// let through as a trial (half-open) while every concurrent one is still
// skipped; the trial's outcome closes the breaker or opens it for another
// cooldown.
//
// Only unavailable failures count. Any other outcome — a reply, even one
// that doesn't parse, or a non-outage error such as a 401 — means the
// server answered, so it resets the count and closes the breaker.
//
// Safe for concurrent use: every alert's goroutine goes through the same
// breaker.
type breaker struct {
	threshold int
	cooldown  time.Duration

	mu       sync.Mutex
	state    breakerState
	failures int       // consecutive unavailable failures while closed
	openedAt time.Time // when the current open period started
}

func newBreaker(threshold int, cooldown time.Duration) *breaker {
	if threshold <= 0 || cooldown <= 0 {
		return nil
	}
	return &breaker{threshold: threshold, cooldown: cooldown}
}

// allow reports whether a call may go ahead at now. When it may not,
// retryIn is how long until the breaker lets a trial through (0 while a
// trial is already in flight). A call allowed here must be followed by
// exactly one record.
func (b *breaker) allow(now time.Time) (ok bool, retryIn time.Duration, tr breakerTransition) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case breakerOpen:
		if elapsed := now.Sub(b.openedAt); elapsed < b.cooldown {
			return false, b.cooldown - elapsed, transitionNone
		}
		b.state = breakerHalfOpen
		return true, 0, transitionHalfOpen
	case breakerHalfOpen:
		return false, 0, transitionNone
	default:
		return true, 0, transitionNone
	}
}

// record feeds one allowed call's outcome back in.
func (b *breaker) record(now time.Time, unavailable bool) breakerTransition {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !unavailable {
		b.failures = 0
		if b.state != breakerClosed {
			b.state = breakerClosed
			return transitionClose
		}
		return transitionNone
	}
	switch b.state {
	case breakerHalfOpen:
		b.state = breakerOpen
		b.openedAt = now
		return transitionReopen
	case breakerClosed:
		b.failures++
		if b.failures >= b.threshold {
			b.state = breakerOpen
			b.openedAt = now
			b.failures = 0
			return transitionOpen
		}
	}
	// Already open: a call admitted before the breaker opened finished
	// late. The open period already running stands.
	return transitionNone
}
