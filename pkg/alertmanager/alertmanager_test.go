package alertmanager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCreateSilence(t *testing.T) {
	var receivedPath, receivedAuth string
	var receivedBody silence

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"silenceID":"abc-123"}`))
	}))
	defer server.Close()

	c := NewClient(Config{Endpoint: server.URL, Username: "op", Password: "secret"})
	matchers := []Matcher{NewExactMatcher("alertname", "SSLCertExpiringSoon"), NewExactMatcher("host", "web01")}
	id, err := c.CreateSilence(context.Background(), matchers, 720*time.Hour, "victoria-gateway", "confirmed 5 times as known noise")
	if err != nil {
		t.Fatalf("CreateSilence: %v", err)
	}
	if id != "abc-123" {
		t.Errorf("id = %q, want abc-123", id)
	}
	if receivedPath != "/api/v2/silences" {
		t.Errorf("path = %q", receivedPath)
	}
	if receivedAuth == "" {
		t.Error("expected Basic Auth header to be sent")
	}
	if len(receivedBody.Matchers) != 2 || receivedBody.Matchers[0].Value != "SSLCertExpiringSoon" {
		t.Errorf("matchers = %+v", receivedBody.Matchers)
	}
	if !receivedBody.EndsAt.After(receivedBody.StartsAt) {
		t.Errorf("EndsAt %v must be after StartsAt %v", receivedBody.EndsAt, receivedBody.StartsAt)
	}
	if receivedBody.CreatedBy != "victoria-gateway" {
		t.Errorf("CreatedBy = %q", receivedBody.CreatedBy)
	}
}

func TestCreateSilence_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"invalid matcher"}`))
	}))
	defer server.Close()

	c := NewClient(Config{Endpoint: server.URL})
	_, err := c.CreateSilence(context.Background(), []Matcher{NewExactMatcher("alertname", "x")}, time.Hour, "victoria-gateway", "")
	if err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

func TestActiveSilenceExists_MatchFound(t *testing.T) {
	matchers := []Matcher{NewExactMatcher("alertname", "SSLCertExpiringSoon"), NewExactMatcher("host", "web01")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]silenceStatus{
			{
				// Reversed order on purpose — matchersEqual must be
				// order-independent, since Alertmanager doesn't guarantee
				// it echoes matchers back in submission order.
				Matchers: []Matcher{matchers[1], matchers[0]},
				Status: struct {
					State string `json:"state"`
				}{State: "active"},
			},
		})
	}))
	defer server.Close()

	c := NewClient(Config{Endpoint: server.URL})
	found, err := c.ActiveSilenceExists(context.Background(), matchers)
	if err != nil {
		t.Fatalf("ActiveSilenceExists: %v", err)
	}
	if !found {
		t.Error("expected an active matching silence to be found")
	}
}

func TestActiveSilenceExists_ExpiredIsIgnored(t *testing.T) {
	matchers := []Matcher{NewExactMatcher("alertname", "SSLCertExpiringSoon")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]silenceStatus{
			{Matchers: matchers, Status: struct {
				State string `json:"state"`
			}{State: "expired"}},
		})
	}))
	defer server.Close()

	c := NewClient(Config{Endpoint: server.URL})
	found, err := c.ActiveSilenceExists(context.Background(), matchers)
	if err != nil {
		t.Fatalf("ActiveSilenceExists: %v", err)
	}
	if found {
		t.Error("an expired silence must not count as active")
	}
}

func TestActiveSilenceExists_NoMatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]silenceStatus{
			{
				Matchers: []Matcher{NewExactMatcher("alertname", "SomethingElse")},
				Status: struct {
					State string `json:"state"`
				}{State: "active"},
			},
		})
	}))
	defer server.Close()

	c := NewClient(Config{Endpoint: server.URL})
	found, err := c.ActiveSilenceExists(context.Background(), []Matcher{NewExactMatcher("alertname", "SSLCertExpiringSoon")})
	if err != nil {
		t.Fatalf("ActiveSilenceExists: %v", err)
	}
	if found {
		t.Error("a non-matching silence must not count as found")
	}
}
