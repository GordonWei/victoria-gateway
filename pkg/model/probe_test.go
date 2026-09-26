package model

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOpenAIClient_Probe_Accepts2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("probe hit %s %s, want GET /v1/models", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewOpenAIClient(OpenAIClientConfig{Endpoint: srv.URL, Model: "m"})
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !c.Available() {
		t.Error("Available() = false for a 204, want any 2xx accepted")
	}
}

func TestOpenAIClient_Probe_Non2xxIsHTTPStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model loading", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewOpenAIClient(OpenAIClientConfig{Endpoint: srv.URL, Model: "m", Backend: "b"})
	err := c.Probe(context.Background())
	var se *HTTPStatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusServiceUnavailable || se.Body != "model loading" {
		t.Fatalf("Probe err = %v, want HTTPStatusError 503 with body", err)
	}
	if c.Available() {
		t.Error("Available() = true for a 503")
	}
}

// A server that accepts the connection but never answers — the sleeping
// host case — must be cut off by the context, not the client Timeout.
func TestOpenAIClient_Probe_HonorsContextDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	c := NewOpenAIClient(OpenAIClientConfig{Endpoint: srv.URL, Model: "m", Timeout: time.Minute})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.Probe(ctx)
	if err == nil {
		t.Fatal("Probe succeeded against a server that never answers")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Probe err = %v, want context.DeadlineExceeded in the chain", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Probe took %v, want it bounded by the 100ms context", d)
	}
}
