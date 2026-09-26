package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/maintenance"
	"github.com/gordonwei/victoria-gateway/pkg/model"
)

// alertRoute is what one alert resolves to: which log backend to query
// and which model (if any) to escalate to. Names are carried along only
// for logging — the per-alert "went via route X" line is how an operator
// debugging a hybrid setup tells which backend actually answered.
type alertRoute struct {
	name           string // "" in legacy (non-hybrid) mode
	logs           aiops.LogSource
	logSourceName  string
	cloud          model.LLM // nil means this route never escalates
	escalationName string
}

// hybridRouter picks an alertRoute per alert from hybrid_routes: first
// route whose matchers all match wins, the validated-last default route
// catches the rest. Same evaluation model as pkg/notify.Router, same glob
// matcher as maintenance windows, so the three label-matching features in
// config.yaml can't disagree about what a pattern means.
type hybridRouter struct {
	routes []hybridRoute
}

type hybridRoute struct {
	matchers  map[string]string
	isDefault bool
	target    alertRoute
}

// pick returns the route for an alert's labels. Validate guarantees a
// default route exists, so the fallthrough at the end is unreachable for
// a validated config; it returns an empty route (no logs, no cloud)
// rather than panicking if that guarantee is ever broken.
func (r *hybridRouter) pick(labels map[string]string) alertRoute {
	for _, rt := range r.routes {
		if rt.isDefault || maintenance.MatchLabels(rt.matchers, labels) {
			return rt.target
		}
	}
	return alertRoute{}
}

// buildHybridRouter constructs every named log source and escalation
// target once (routes sharing a name share the client) and wires them
// into routes. cfg.Validate is assumed to have run: undefined names,
// missing default, etc. are already rejected there.
func buildHybridRouter(cfg *config.Config) (*hybridRouter, error) {
	sources := map[string]aiops.LogSource{}
	for name, ls := range cfg.LogSources {
		src, err := buildLogSourceEntry("log_sources."+name, ls, cfg.Loki.Endpoint)
		if err != nil {
			return nil, err
		}
		sources[name] = src
	}
	targets := map[string]model.LLM{}
	for name, t := range cfg.EscalationTargets {
		c, err := buildCloud("escalation_targets."+name, t)
		if err != nil {
			return nil, err
		}
		targets[name] = c
	}
	r := &hybridRouter{}
	for i, hr := range cfg.HybridRoutes {
		name := fmt.Sprintf("hybrid_routes[%d]", i)
		if hr.Default {
			name = "default"
		}
		src, ok := sources[hr.LogSource]
		if !ok {
			return nil, fmt.Errorf("%s: log_source %q is not defined under log_sources", name, hr.LogSource)
		}
		target := alertRoute{name: name, logs: src, logSourceName: hr.LogSource}
		if hr.Escalation != "" {
			c, ok := targets[hr.Escalation]
			if !ok {
				return nil, fmt.Errorf("%s: escalation %q is not defined under escalation_targets", name, hr.Escalation)
			}
			target.cloud = c
			target.escalationName = hr.Escalation
		}
		r.routes = append(r.routes, hybridRoute{matchers: hr.Matchers, isDefault: hr.Default, target: target})
	}
	return r, nil
}

// hybridSummary renders the startup line describing a hybrid setup, e.g.
// "hybrid: aws(cloudwatch) gcp(gcp_logging) onprem(loki) | escalation:
// aws(aws-devops-agent) default(gemini) | 3 routes".
func hybridSummary(cfg *config.Config) string {
	var srcs, tgts []string
	for name, ls := range cfg.LogSources {
		typ := "loki"
		if ls != nil && ls.Type != "" {
			typ = ls.Type
		}
		srcs = append(srcs, fmt.Sprintf("%s(%s)", name, typ))
	}
	for name, t := range cfg.EscalationTargets {
		p := t.Provider
		if p == "" {
			p = "gemini"
		}
		tgts = append(tgts, fmt.Sprintf("%s(%s)", name, p))
	}
	sort.Strings(srcs)
	sort.Strings(tgts)
	return fmt.Sprintf("hybrid: %s | escalation: %s | %d routes",
		strings.Join(srcs, " "), strings.Join(tgts, " "), len(cfg.HybridRoutes))
}
