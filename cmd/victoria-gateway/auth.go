// auth.go guards the read/write web pages — /incidents, /pending,
// /maintenance-windows — separately from the webhook's own
// checkWebhookAuth (see main.go). It exists because these endpoints
// started out purely read-only (a link to click from a notification, on
// a private network — see the README's "Securing the web UI" section for
// the reasoning that made that an acceptable default at the time), but
// gained a POST confirm form once /pending shipped. A writable endpoint
// changes the risk calculus enough that operators need a real way to
// lock it down, without this package deciding that every deployment must.
package main

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/gordonwei/victoria-gateway/pkg/config"
)

// AuthMiddleware wraps an http.Handler with whatever access check a
// deployment wants in front of the web UI. It's a type, not a concrete
// struct, so the one implementation shipped today (HTTP Basic Auth, via
// newBasicAuthMiddleware) can later be swapped for something else — an
// OIDC/SSO reverse-proxy check, a bearer-token check, anything — without
// touching a single handler in incidents.go or pending.go: every web
// route is registered through wrapWebUI (see main.go), which is the only
// place that knows which AuthMiddleware is active.
type AuthMiddleware func(http.Handler) http.Handler

// noAuthMiddleware passes every request through unchanged. This is the
// default — cfg.WebUIAuth == nil — preserving the exact pre-existing
// behavior of /incidents and /maintenance-windows for anyone who hasn't
// opted into locking them down.
func noAuthMiddleware(next http.Handler) http.Handler { return next }

// newBasicAuthMiddleware returns an AuthMiddleware enforcing HTTP Basic
// Auth against cfg's username/password, using the same constant-time
// comparison as checkWebhookAuth so a timing side-channel can't be used
// to guess a credential one byte at a time. This is today's only
// AuthMiddleware implementation; see the type's doc comment for how a
// future SSO/OIDC-based one would plug in at the same seam.
func newBasicAuthMiddleware(cfg *config.WebhookAuthConfig) AuthMiddleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := r.BasicAuth()
			userOK := ok && subtle.ConstantTimeCompare([]byte(user), []byte(cfg.Username)) == 1
			passOK := ok && subtle.ConstantTimeCompare([]byte(pass), []byte(cfg.Password)) == 1
			if !userOK || !passOK {
				w.Header().Set("WWW-Authenticate", `Basic realm="victoria-gateway"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// webUIAuthMiddleware picks the AuthMiddleware for cfg.WebUIAuth: basic
// auth if configured, otherwise noAuthMiddleware (today's default
// behavior, unchanged).
func webUIAuthMiddleware(cfg *config.WebhookAuthConfig) AuthMiddleware {
	if cfg == nil {
		return noAuthMiddleware
	}
	return newBasicAuthMiddleware(cfg)
}

// actorFromRequest identifies who's making a web-UI request, for
// pkg/audit's Entry.Actor. authenticated must be true only when
// webUIAuthMiddleware actually verified this request's Basic Auth
// credentials before it reached the handler — only then is the header's
// username trustworthy enough to record. When webui_auth isn't
// configured, nothing has checked that header at all: a caller can set
// any `Authorization: Basic ...` value they like, so trusting it here
// (an earlier version of this function did, unconditionally) would let
// anyone forge the audit trail's actor field. In that case this falls
// back to the caller's remote IP instead — not a real identity, but not
// forgeable by the request itself either, and consistent with the
// enterprise-features draft's fallback of "at least record the source
// IP" when no identity system exists.
func actorFromRequest(r *http.Request, authenticated bool) string {
	if authenticated {
		if user, _, ok := r.BasicAuth(); ok && user != "" {
			return user
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if host == "" {
		return "unknown"
	}
	return "ip:" + host
}

// sameOriginOnly rejects state-changing requests (anything but GET, HEAD,
// OPTIONS) that a browser marks as coming from another site. Basic Auth
// alone doesn't stop that: a browser that has cached the credentials
// attaches them to a cross-site form POST as readily as to one from our
// own page, so a link on any other site could confirm a pending incident
// or replace maintenance windows in the operator's name.
//
// The check uses what browsers already send rather than a CSRF token:
// Sec-Fetch-Site (every current browser) must be "same-origin" or "none"
// (typed into the address bar / a bookmark); failing that, Origin, then
// Referer, must name this same host — either the Host the request
// arrived with, or, behind a reverse proxy that rewrites Host, the
// scheme and host of rag.public_base_url (the address operators actually
// open the pages at). A request with none of the three — curl, scripts,
// the maintenance-window API's usual callers — is not a browser acting
// on someone's behalf and passes through unchanged.
func sameOriginOnly(next http.Handler, publicBaseURL string) http.Handler {
	public := parsePublicOrigin(publicBaseURL)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !crossSiteWrite(r, public) {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "cross-site request refused", http.StatusForbidden)
	})
}

// parsePublicOrigin returns publicBaseURL as a URL usable for an origin
// comparison, or nil when it's unset or has no scheme and host (in which
// case only the request's own Host counts as same-origin).
func parsePublicOrigin(publicBaseURL string) *url.URL {
	if publicBaseURL == "" {
		return nil
	}
	u, err := url.Parse(publicBaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil
	}
	return u
}

func crossSiteWrite(r *http.Request, public *url.URL) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site != "same-origin" && site != "none"
	}
	for _, h := range []string{"Origin", "Referer"} {
		v := r.Header.Get(h)
		if v == "" {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || u.Host == "" {
			return true // includes Origin: null
		}
		if strings.EqualFold(u.Host, r.Host) {
			return false
		}
		if public != nil && strings.EqualFold(u.Scheme, public.Scheme) && strings.EqualFold(u.Host, public.Host) {
			return false
		}
		return true
	}
	return false
}

// webUIChain wraps a web page handler with the CSRF check and then the
// configured auth. Every web UI route goes through this (see runServe).
// publicBaseURL is rag.public_base_url ("" when unset); see
// sameOriginOnly.
func webUIChain(auth AuthMiddleware, publicBaseURL string) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler { return auth(sameOriginOnly(h, publicBaseURL)) }
}
