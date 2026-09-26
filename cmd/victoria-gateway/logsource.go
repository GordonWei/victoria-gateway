package main

import (
	"fmt"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/cloudwatch"
	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/gcplogging"
)

// logSourceType returns cfg.LogSource.Type, defaulting to "loki" when the
// whole block (or just Type) is left unset — the pre-LogSource behavior.
func logSourceType(cfg *config.Config) string {
	if cfg.LogSource != nil && cfg.LogSource.Type != "" {
		return cfg.LogSource.Type
	}
	return "loki"
}

// buildLogSource constructs whichever backend cfg.LogSource.Type selects
// (the legacy single-source path, used when hybrid_routes is unset).
// cfg.Validate is assumed to have already rejected any type other than
// "loki"/"cloudwatch"/"gcp_logging" and confirmed the matching block's
// required fields are present — this only returns an error for the
// "shouldn't happen" case of Validate not having been called, so callers
// that do call Validate first (runServe does) can treat that path as
// unreachable in practice.
func buildLogSource(cfg *config.Config) (aiops.LogSource, error) {
	return buildLogSourceEntry("log_source", cfg.LogSource, cfg.Loki.Endpoint)
}

// buildLogSourceEntry constructs one log backend from a single log source
// block — the legacy top-level log_source (label "log_source") or one
// named entry of log_sources (label "log_sources.<name>"). ls may be nil,
// which means type "loki". A type-"loki" entry uses its own
// loki.endpoint if set, otherwise defaultLokiEndpoint (the top-level
// loki.endpoint).
func buildLogSourceEntry(label string, ls *config.LogSourceConfig, defaultLokiEndpoint string) (aiops.LogSource, error) {
	typ := "loki"
	if ls != nil && ls.Type != "" {
		typ = ls.Type
	}
	switch typ {
	case "loki":
		endpoint := defaultLokiEndpoint
		if ls != nil && ls.Loki != nil && ls.Loki.Endpoint != "" {
			endpoint = ls.Loki.Endpoint
		}
		return aiops.NewLokiLogSource(aiops.NewClient(endpoint)), nil
	case "cloudwatch":
		cw := ls.CloudWatch
		if cw == nil {
			return nil, fmt.Errorf("%s.type is \"cloudwatch\" but %s.cloudwatch is not set", label, label)
		}
		return cloudwatch.NewClient(cloudwatch.ClientConfig{
			Region:        cw.Region,
			LogGroupNames: cw.LogGroupNames,
			QueryTemplate: cw.QueryTemplate,
			Timeout:       time.Duration(cw.TimeoutSec) * time.Second,
		}), nil
	case "gcp_logging":
		gl := ls.GCPLogging
		if gl == nil {
			return nil, fmt.Errorf("%s.type is \"gcp_logging\" but %s.gcp_logging is not set", label, label)
		}
		return gcplogging.NewClient(gcplogging.ClientConfig{
			ProjectID:      gl.ProjectID,
			FilterTemplate: gl.FilterTemplate,
			Timeout:        time.Duration(gl.TimeoutSec) * time.Second,
		}), nil
	default:
		return nil, fmt.Errorf("%s.type %q is not one of \"loki\", \"cloudwatch\", \"gcp_logging\"", label, typ)
	}
}
