package main

import (
	"net/http/httptest"
	"testing"
)

func TestActorFromRequest_Authenticated_UsesBasicAuthUsername(t *testing.T) {
	req := httptest.NewRequest("POST", "/pending/1", nil)
	req.SetBasicAuth("alice", "whatever")

	if got := actorFromRequest(req, true); got != "alice" {
		t.Errorf("actorFromRequest = %q, want %q", got, "alice")
	}
}

// TestActorFromRequest_Unauthenticated_IgnoresForgedHeader is the
// regression test for the actor-spoofing bug: when nothing has actually
// verified the request's Basic Auth header (webui_auth/webhook_auth not
// configured), that header must never be trusted as the audit actor —
// a caller can set any Authorization value it wants. Before this fix,
// actorFromRequest read r.BasicAuth() unconditionally, letting anyone
// forge the audit trail by sending a header no middleware ever checked.
func TestActorFromRequest_Unauthenticated_IgnoresForgedHeader(t *testing.T) {
	req := httptest.NewRequest("POST", "/pending/1", nil)
	req.RemoteAddr = "192.0.2.5:54321"
	req.SetBasicAuth("admin", "not-a-real-password-nothing-checked-this")

	got := actorFromRequest(req, false)
	if got == "admin" {
		t.Fatalf("actorFromRequest = %q — an unverified Authorization header must never be trusted as the actor", got)
	}
	if got != "ip:192.0.2.5" {
		t.Errorf("actorFromRequest = %q, want ip:192.0.2.5 (fall back to remote IP)", got)
	}
}

func TestActorFromRequest_Authenticated_NoHeaderFallsBackToIP(t *testing.T) {
	req := httptest.NewRequest("POST", "/pending/1", nil)
	req.RemoteAddr = "203.0.113.9:1234"

	if got := actorFromRequest(req, true); got != "ip:203.0.113.9" {
		t.Errorf("actorFromRequest = %q, want ip:203.0.113.9", got)
	}
}

func TestActorFromRequest_NoPortInRemoteAddr(t *testing.T) {
	req := httptest.NewRequest("POST", "/pending/1", nil)
	req.RemoteAddr = "not-a-host-port-pair"

	if got := actorFromRequest(req, false); got != "ip:not-a-host-port-pair" {
		t.Errorf("actorFromRequest = %q, want the raw RemoteAddr prefixed with ip:", got)
	}
}
