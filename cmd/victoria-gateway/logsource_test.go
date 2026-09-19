package main

import (
	"testing"

	"github.com/gordonwei/victoria-gateway/pkg/config"
)

func TestLogSourceType_NilBlockDefaultsToLoki(t *testing.T) {
	if got := logSourceType(&config.Config{}); got != "loki" {
		t.Errorf("logSourceType(nil block) = %q, want %q", got, "loki")
	}
}

func TestLogSourceType_EmptyTypeDefaultsToLoki(t *testing.T) {
	cfg := &config.Config{LogSource: &config.LogSourceConfig{}}
	if got := logSourceType(cfg); got != "loki" {
		t.Errorf("logSourceType(empty Type) = %q, want %q", got, "loki")
	}
}

func TestLogSourceType_ExplicitType(t *testing.T) {
	cfg := &config.Config{LogSource: &config.LogSourceConfig{Type: "cloudwatch"}}
	if got := logSourceType(cfg); got != "cloudwatch" {
		t.Errorf("logSourceType = %q, want %q", got, "cloudwatch")
	}
}

func TestBuildLogSource_DefaultsToLoki(t *testing.T) {
	cfg := &config.Config{Loki: config.LokiConfig{Endpoint: "http://loki:3100"}}
	src, err := buildLogSource(cfg)
	if err != nil {
		t.Fatalf("buildLogSource: %v", err)
	}
	if src == nil {
		t.Fatal("expected a non-nil LogSource for the loki default")
	}
}

func TestBuildLogSource_CloudWatch(t *testing.T) {
	cfg := &config.Config{
		LogSource: &config.LogSourceConfig{
			Type: "cloudwatch",
			CloudWatch: &config.CloudWatchConfig{
				Region:        "us-east-1",
				LogGroupNames: []string{"/aws/lambda/my-fn"},
			},
		},
	}
	src, err := buildLogSource(cfg)
	if err != nil {
		t.Fatalf("buildLogSource: %v", err)
	}
	if src == nil {
		t.Fatal("expected a non-nil LogSource for cloudwatch")
	}
}

func TestBuildLogSource_CloudWatch_NilBlockErrors(t *testing.T) {
	cfg := &config.Config{LogSource: &config.LogSourceConfig{Type: "cloudwatch"}}
	if _, err := buildLogSource(cfg); err == nil {
		t.Error("expected an error when log_source.type is cloudwatch but log_source.cloudwatch is nil")
	}
}

func TestBuildLogSource_GCPLogging(t *testing.T) {
	cfg := &config.Config{
		LogSource: &config.LogSourceConfig{
			Type:       "gcp_logging",
			GCPLogging: &config.GCPLoggingConfig{ProjectID: "my-project"},
		},
	}
	src, err := buildLogSource(cfg)
	if err != nil {
		t.Fatalf("buildLogSource: %v", err)
	}
	if src == nil {
		t.Fatal("expected a non-nil LogSource for gcp_logging")
	}
}

func TestBuildLogSource_GCPLogging_NilBlockErrors(t *testing.T) {
	cfg := &config.Config{LogSource: &config.LogSourceConfig{Type: "gcp_logging"}}
	if _, err := buildLogSource(cfg); err == nil {
		t.Error("expected an error when log_source.type is gcp_logging but log_source.gcp_logging is nil")
	}
}

func TestBuildLogSource_UnknownType(t *testing.T) {
	cfg := &config.Config{LogSource: &config.LogSourceConfig{Type: "splunk"}}
	if _, err := buildLogSource(cfg); err == nil {
		t.Error("expected an error for an unrecognized log_source.type")
	}
}
