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
	"net/http"

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
