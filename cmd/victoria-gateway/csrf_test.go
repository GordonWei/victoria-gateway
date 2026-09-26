package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

func TestCrossSiteWrite(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		headers map[string]string
		want    bool
	}{
		{"GET is never blocked", "GET", map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"same-origin POST", "POST", map[string]string{"Sec-Fetch-Site": "same-origin"}, false},
		{"typed-in navigation", "POST", map[string]string{"Sec-Fetch-Site": "none"}, false},
		{"cross-site POST", "POST", map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		{"same-site but other origin", "POST", map[string]string{"Sec-Fetch-Site": "same-site"}, true},
		{"Sec-Fetch-Site wins over a matching Origin", "POST", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://vg.local:8090"}, true},
		{"Origin matches Host", "POST", map[string]string{"Origin": "http://vg.local:8090"}, false},
		{"Origin other host", "PUT", map[string]string{"Origin": "https://evil.example"}, true},
		{"Origin null", "POST", map[string]string{"Origin": "null"}, true},
		{"Referer other host", "POST", map[string]string{"Referer": "https://evil.example/page"}, true},
		{"Referer same host", "POST", map[string]string{"Referer": "http://vg.local:8090/pending/1"}, false},
		{"no browser headers (curl)", "PUT", nil, false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, "http://vg.local:8090/pending/1", nil)
		for k, v := range tc.headers {
			r.Header.Set(k, v)
		}
		if got := crossSiteWrite(r); got != tc.want {
			t.Errorf("%s: crossSiteWrite = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestWebUIChain_PendingConfirm drives the real /pending/{id} handler
// through the same wrapping runServe uses: a cross-site POST is refused
// before it can confirm anything, a same-origin one goes through, with
// and without webui_auth.
func TestWebUIChain_PendingConfirm(t *testing.T) {
	for _, auth := range []*config.WebhookAuthConfig{nil, {Username: "u", Password: "p"}} {
		store := &fakeRAGStore{pending: []rag.Record{{ID: 9, AlertName: "A", Host: "h", Summary: "s", CreatedAt: time.Now()}}}
		h := &handler{rag: store}
		wrapped := webUIChain(webUIAuthMiddleware(auth))(http.HandlerFunc(h.handlePendingDetail))

		post := func(site string) *httptest.ResponseRecorder {
			form := url.Values{"resolution": {"fixed"}}
			req := httptest.NewRequest(http.MethodPost, "/pending/9", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Sec-Fetch-Site", site)
			req.SetBasicAuth("u", "p") // a browser replays cached credentials cross-site too
			rec := httptest.NewRecorder()
			wrapped.ServeHTTP(rec, req)
			return rec
		}
		if rec := post("cross-site"); rec.Code != http.StatusForbidden {
			t.Fatalf("auth=%v cross-site POST status = %d, want 403", auth != nil, rec.Code)
		}
		if len(store.confirmPendingCalls) != 0 {
			t.Fatalf("auth=%v: cross-site POST confirmed the record", auth != nil)
		}
		if rec := post("same-origin"); rec.Code != http.StatusOK {
			t.Fatalf("auth=%v same-origin POST status = %d, want 200", auth != nil, rec.Code)
		}
		if len(store.confirmPendingCalls) != 1 {
			t.Errorf("auth=%v: same-origin POST did not confirm", auth != nil)
		}
	}
}

func TestWebUIChain_BatchAndMaintenanceRefuseCrossSite(t *testing.T) {
	h := &handler{rag: &fakeRAGStore{}}
	for _, tc := range []struct {
		method, path string
		handler      http.HandlerFunc
	}{
		{http.MethodPost, "/pending/batch", h.handlePendingBatch},
		{http.MethodPut, "/maintenance-windows", h.handleMaintenanceWindows},
	} {
		wrapped := webUIChain(noAuthMiddleware)(tc.handler)
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("[]"))
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s cross-site: status = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
}
