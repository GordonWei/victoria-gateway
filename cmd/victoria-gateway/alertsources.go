package main

import (
	"log"
	"net/http"
	"strings"

	"github.com/gordonwei/victoria-gateway/pkg/alertsource"
	"github.com/gordonwei/victoria-gateway/pkg/config"
)

// registerAlertSources adds the optional non-Alertmanager webhook
// endpoints. Nothing is registered unless alert_sources configures it,
// so an unconfigured gateway answers these paths with 404 exactly as it
// did before they existed. Both endpoints use webhook_auth the same way
// /webhook/alertmanager does.
func registerAlertSources(mux *http.ServeMux, h *handler, ac *config.AlertSourcesConfig) {
	if ac == nil {
		return
	}
	if ac.Grafana != nil && ac.Grafana.Enabled {
		mux.HandleFunc("/webhook/grafana", func(w http.ResponseWriter, r *http.Request) {
			h.handleAlertWebhook(w, r, "grafana", alertsource.ParseGrafana)
		})
		log.Printf("alert sources: POST /webhook/grafana enabled")
	}
	if len(ac.Generic) == 0 {
		return
	}
	sources := make(map[string]alertsource.Generic, len(ac.Generic))
	names := make([]string, 0, len(ac.Generic))
	for _, g := range ac.Generic {
		sources[g.Name] = alertsource.Generic{Cfg: g}
		names = append(names, g.Name)
	}
	mux.HandleFunc("/webhook/generic/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/webhook/generic/")
		src, ok := sources[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h.handleAlertWebhook(w, r, "generic/"+name, src.Parse)
	})
	log.Printf("alert sources: POST /webhook/generic/{%s} enabled", strings.Join(names, ","))
}
