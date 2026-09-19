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

// buildLogSource constructs whichever backend cfg.LogSource.Type selects.
// cfg.Validate is assumed to have already rejected any type other than
// "loki"/"cloudwatch"/"gcp_logging" and confirmed the matching block's
// required fields are present — this only returns an error for the
// "shouldn't happen" case of Validate not having been called, so callers
// that do call Validate first (runServe does) can treat that path as
// unreachable in practice.
func buildLogSource(cfg *config.Config) (aiops.LogSource, error) {
	switch logSourceType(cfg) {
	case "loki":
		return aiops.NewLokiLogSource(aiops.NewClient(cfg.Loki.Endpoint)), nil
	case "cloudwatch":
		cw := cfg.LogSource.CloudWatch
		if cw == nil {
			return nil, fmt.Errorf("log_source.type is \"cloudwatch\" but log_source.cloudwatch is not set")
		}
		return cloudwatch.NewClient(cloudwatch.ClientConfig{
			Region:        cw.Region,
			LogGroupNames: cw.LogGroupNames,
			QueryTemplate: cw.QueryTemplate,
			Timeout:       time.Duration(cw.TimeoutSec) * time.Second,
		}), nil
	case "gcp_logging":
		gl := cfg.LogSource.GCPLogging
		if gl == nil {
			return nil, fmt.Errorf("log_source.type is \"gcp_logging\" but log_source.gcp_logging is not set")
		}
		return gcplogging.NewClient(gcplogging.ClientConfig{
			ProjectID:      gl.ProjectID,
			FilterTemplate: gl.FilterTemplate,
			Timeout:        time.Duration(gl.TimeoutSec) * time.Second,
		}), nil
	default:
		return nil, fmt.Errorf("log_source.type %q is not one of \"loki\", \"cloudwatch\", \"gcp_logging\"", logSourceType(cfg))
	}
}
