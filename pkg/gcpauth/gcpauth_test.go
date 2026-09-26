package gcpauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// A token endpoint that never answers must fail the token fetch after the
// timeout instead of hanging on http.DefaultClient.
func TestFindDefaultCredentials_TokenFetchIsBounded(t *testing.T) {
	release := make(chan struct{})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	// An authorized_user ADC file takes its token endpoint from token_uri,
	// so the refresh goes to the stuck server above.
	adc, err := json.Marshal(map[string]string{
		"type":          "authorized_user",
		"client_id":     "id",
		"client_secret": "secret",
		"refresh_token": "refresh",
		"token_uri":     srv.URL + "/token",
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(path, adc, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)

	creds, err := findDefaultCredentials(100*time.Millisecond, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		t.Fatalf("findDefaultCredentials: %v", err)
	}

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := creds.TokenSource.Token()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Token() succeeded against an endpoint that never answers")
		}
		if !strings.Contains(err.Error(), "Client.Timeout") && !strings.Contains(err.Error(), "deadline exceeded") {
			t.Errorf("err = %v, want the HTTP client's timeout", err)
		}
		if el := time.Since(start); el > 5*time.Second {
			t.Errorf("Token() took %s, want it bounded by the 100ms client timeout", el)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Token() still blocked after 10s: the refresh isn't using the timeout client")
	}
	if hits.Load() == 0 {
		t.Error("the token endpoint was never called")
	}
}

func TestFindDefaultCredentials_DefaultTimeout(t *testing.T) {
	c, ok := tokenContext(TokenTimeout).Value(oauth2.HTTPClient).(*http.Client)
	if !ok || c.Timeout != 30*time.Second {
		t.Errorf("token context client = %+v, want an *http.Client with a 30s timeout", c)
	}
}
