// Package tgactions handles presses of the inline buttons victoria-gateway
// can attach to a Telegram notification (acknowledge, escalate now,
// silence). It is the part that has to be right for the feature to be
// safe, so it is kept free of the rest of the service:
//
//   - Signer puts an HMAC on every button's callback data, so data that
//     did not come from this process (a forged or edited callback) is
//     refused before anything is looked up.
//   - Store keeps a server-side, single-use nonce per button with an
//     expiry. The callback data carries only the nonce; which action it
//     is and what it applies to live here, so a client can't change
//     either, and a replayed or double-clicked press finds the nonce
//     already used.
//   - Client talks to the Bot API (getUpdates long polling — no inbound
//     webhook — answerCallbackQuery, sendMessage).
//   - Processor applies the checks in order (chat, user allowlist,
//     signature, nonce) and only then hands the press to an Executor,
//     auditing every outcome, refusals included.
//
// Nothing here survives a restart: buttons from before a restart simply
// stop working, which is the safe direction.
package tgactions

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"
)

// Kind is what a button does.
type Kind string

const (
	KindAck      Kind = "ack"
	KindEscalate Kind = "escalate"
	KindSilence  Kind = "silence"
)

// Errors returned by Signer.Verify and Store.Claim.
var (
	ErrBadSignature = errors.New("tgactions: callback data signature does not match")
	ErrUnknown      = errors.New("tgactions: unknown or expired button")
	ErrUsed         = errors.New("tgactions: button already used")
)

const (
	dataVersion = "v1"
	nonceBytes  = 9  // 12 base64url characters
	macBytes    = 12 // 16 base64url characters
)

var b64 = base64.RawURLEncoding

// Signer signs and verifies callback data. Telegram limits callback data
// to 64 bytes; the encoded form is 32.
type Signer struct{ key []byte }

// NewSigner returns a Signer keyed with secret.
func NewSigner(secret string) *Signer { return &Signer{key: []byte(secret)} }

func (s *Signer) mac(nonce string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(dataVersion + ":" + nonce))
	return b64.EncodeToString(m.Sum(nil)[:macBytes])
}

// Sign returns the callback data for nonce: "v1:<nonce>:<mac>".
func (s *Signer) Sign(nonce string) string {
	return dataVersion + ":" + nonce + ":" + s.mac(nonce)
}

// Verify checks data's signature and returns its nonce.
func (s *Signer) Verify(data string) (string, error) {
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != dataVersion || len(parts[1]) != b64.EncodedLen(nonceBytes) {
		return "", ErrBadSignature
	}
	if !hmac.Equal([]byte(parts[2]), []byte(s.mac(parts[1]))) {
		return "", ErrBadSignature
	}
	return parts[1], nil
}

// Store holds issued buttons. The zero value is not usable; see NewStore.
type Store struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	now     func() time.Time
	entries map[string]*entry
	order   []string // issue order, for evicting the oldest when full
}

type entry struct {
	kind    Kind
	payload any
	expires time.Time
	used    bool
}

// NewStore returns a Store whose buttons expire after ttl, holding at most
// max buttons (the oldest are dropped first once full). now may be nil.
func NewStore(ttl time.Duration, max int, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{ttl: ttl, max: max, now: now, entries: make(map[string]*entry)}
}

// Issue registers one button per kind, all sharing payload, and returns
// each one's nonce.
func (s *Store) Issue(kinds []Kind, payload any) (map[Kind]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	out := make(map[Kind]string, len(kinds))
	exp := s.now().Add(s.ttl)
	for _, k := range kinds {
		buf := make([]byte, nonceBytes)
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		n := b64.EncodeToString(buf)
		s.entries[n] = &entry{kind: k, payload: payload, expires: exp}
		s.order = append(s.order, n)
		out[k] = n
	}
	for len(s.entries) > s.max && len(s.order) > 0 {
		delete(s.entries, s.order[0])
		s.order = s.order[1:]
	}
	return out, nil
}

// Claim marks nonce used and returns its kind and payload. A nonce that
// was never issued, was evicted, or has expired is ErrUnknown; one already
// claimed is ErrUsed. Only the first of several concurrent claims wins.
func (s *Store) Claim(nonce string) (Kind, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[nonce]
	if !ok || !s.now().Before(e.expires) {
		return "", nil, ErrUnknown
	}
	if e.used {
		return "", nil, ErrUsed
	}
	e.used = true
	return e.kind, e.payload, nil
}

// Release makes a claimed, unexpired nonce usable again: for an action
// that was refused for a reason that can pass later (the escalation rate
// limit), so the same button can be pressed again within its lifetime.
func (s *Store) Release(nonce string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[nonce]; ok && s.now().Before(e.expires) {
		e.used = false
	}
}

// Len reports how many buttons are held (expired ones included until the
// next Issue prunes them).
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func (s *Store) pruneLocked() {
	now := s.now()
	kept := s.order[:0]
	for _, n := range s.order {
		e, ok := s.entries[n]
		if !ok {
			continue
		}
		if !now.Before(e.expires) {
			delete(s.entries, n)
			continue
		}
		kept = append(kept, n)
	}
	s.order = kept
}
