package main

import (
	"net/http"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/metrics"
)

// newHTTPServer is the http.Server shape every listener here uses.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:    addr,
		Handler: h,
		// A slow or stalled client mustn't pin a goroutine forever.
		// ReadTimeout is generous because Alertmanager payloads are small
		// and local — it's a stall guard, not a pacing device. No
		// WriteTimeout: in sync mode the response legitimately takes as
		// long as the slowest analysis (minutes on a cloud escalation).
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// registerMetrics wires /metrics according to metrics_listen_addr and
// metrics_auth. With neither set it registers the plain handler on the
// main mux, exactly as before, and returns nil. With metrics_auth the
// handler requires Basic Auth wherever it is served. With
// metrics_listen_addr the main mux gets no /metrics at all (so it 404s)
// and the returned server, which the caller starts and shuts down
// alongside the main one, serves it instead. There is no default
// address for that second listener: binding loopback by default would
// hide /metrics from a Prometheus outside the container.
func registerMetrics(cfg *config.Config, mainMux *http.ServeMux, m *metrics.Counters) *http.Server {
	h := m.Handler()
	if cfg.MetricsAuth != nil {
		h = newBasicAuthMiddleware(cfg.MetricsAuth)(h)
	}
	if cfg.MetricsListenAddr == "" {
		mainMux.Handle("/metrics", h)
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", h)
	return newHTTPServer(cfg.MetricsListenAddr, mux)
}
