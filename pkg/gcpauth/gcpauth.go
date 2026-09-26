// Package gcpauth loads Application Default Credentials for the
// gateway's GCP clients (pkg/model's Vertex AI and Cloud Assist clients,
// pkg/gcplogging) with a bound on every token fetch.
//
// google.FindDefaultCredentials keeps the context it is given and makes
// every later token refresh with that context's oauth2.HTTPClient, or
// http.DefaultClient when there is none. http.DefaultClient has no
// timeout, so a token endpoint that accepts the connection and then
// never answers (an STS or IAM Credentials hiccup, a proxy holding the
// request) would hang the escalation or log query that asked for the
// token, well past the API call's own timeout, which only starts once a
// token exists. The context passed here therefore carries an
// *http.Client with a timeout. It is never cancelled: it lives as long
// as the token source does, which is the life of the process.
package gcpauth

import (
	"context"
	"net/http"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// TokenTimeout bounds one token fetch or refresh.
const TokenTimeout = 30 * time.Second

// FindDefaultCredentials is google.FindDefaultCredentials with every
// token fetch bounded by TokenTimeout.
func FindDefaultCredentials(scopes ...string) (*google.Credentials, error) {
	return findDefaultCredentials(TokenTimeout, scopes...)
}

func findDefaultCredentials(timeout time.Duration, scopes ...string) (*google.Credentials, error) {
	return google.FindDefaultCredentials(tokenContext(timeout), scopes...)
}

// tokenContext is the long-lived context the token source keeps.
func tokenContext(timeout time.Duration) context.Context {
	return context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Timeout: timeout})
}
