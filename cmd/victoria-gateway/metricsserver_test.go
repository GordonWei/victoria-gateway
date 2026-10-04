package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/metrics"
)

func get(t *testing.T, h http.Handler, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The default — neither option set — is the pre-existing behavior: open
// /metrics on the main mux and no second listener.
func TestRegisterMetrics_DefaultUnchanged(t *testing.T) {
	m := &metrics.Counters{}
	m.SetBuildInfo("v-test", "abc")
	mux := http.NewServeMux()
	if srv := registerMetrics(&config.Config{}, mux, m); srv != nil {
		t.Fatalf("second listener started without metrics_listen_addr: %+v", srv)
	}
	rec := get(t, mux, "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `victoria_gateway_build_info{version="v-test",commit="abc"} 1`) {
		t.Errorf("GET /metrics = %d\n%s", rec.Code, rec.Body.String())
	}
}

func TestRegisterMetrics_AuthOnMainPort(t *testing.T) {
	cfg := &config.Config{MetricsAuth: &config.WebhookAuthConfig{Username: "prom", Password: "s3cret"}}
	mux := http.NewServeMux()
	if srv := registerMetrics(cfg, mux, &metrics.Counters{}); srv != nil {
		t.Fatal("unexpected second listener")
	}
	if rec := get(t, mux, "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no credentials: %d, want 401", rec.Code)
	}
	if rec := get(t, mux, "prom", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: %d, want 401", rec.Code)
	}
	if rec := get(t, mux, "prom", "s3cret"); rec.Code != http.StatusOK {
		t.Errorf("right credentials: %d, want 200", rec.Code)
	}
}

func TestRegisterMetrics_SeparateListener(t *testing.T) {
	cfg := &config.Config{
		MetricsListenAddr: "127.0.0.1:0",
		MetricsAuth:       &config.WebhookAuthConfig{Username: "prom", Password: "s3cret"},
	}
	mux := http.NewServeMux()
	srv := registerMetrics(cfg, mux, &metrics.Counters{})
	if srv == nil {
		t.Fatal("no second listener with metrics_listen_addr set")
	}
	if rec := get(t, mux, "prom", "s3cret"); rec.Code != http.StatusNotFound {
		t.Errorf("main port /metrics = %d, want 404 once it moved", rec.Code)
	}
	if srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.IdleTimeout == 0 {
		t.Errorf("metrics server lacks the main server's timeouts: %+v", srv)
	}

	// Serve it for real, scrape, then shut it down the way runServe does.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	url := "http://" + ln.Addr().String() + "/metrics"

	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("second listener without credentials = %d, want 401", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.SetBasicAuth("prom", "s3cret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("second listener with credentials = %d, want 200", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Serve returned %v, want ErrServerClosed", err)
	}
}
