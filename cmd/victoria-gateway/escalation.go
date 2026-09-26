package main

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/model"
)

// reasonLocalUnavailable is the escalation reason used when every local
// summarizer failed and the alert is handed straight to the route's
// escalation target instead of failing outright.
const reasonLocalUnavailable = "local LLM unavailable"

// runEscalation tries the route's escalation steps in order and returns
// the first successful result along with the step that produced it. The
// caller has already charged escalation.max_per_hour once for this alert;
// trying a fallback step does not charge it again — a failed call isn't
// the spend max_per_hour exists to bound, and "one alert, one slot" is
// the rule an operator can actually reason about.
//
// Each failed step is logged and counted in escalation_failures_total;
// escalations_total moves once, on success. With a single step (every
// legacy config without cloud_fallbacks) the log lines and counters are
// exactly what they were before fallbacks existed.
func (h *handler) runEscalation(steps []escalationStep, alert aiops.Alert, logs []aiops.LogEntry, ragContext, reason string) (aiops.SummarizeResult, escalationStep, error) {
	alertName := alert.Labels["alertname"]
	var failures []string
	for i, step := range steps {
		start := time.Now()
		result, err := aiops.SummarizeWithLLM(step.llm, alert, logs, ragContext)
		h.metrics.ObserveCloudLLMDuration(step.display, time.Since(start))
		if err == nil {
			if step.name != "" {
				log.Printf("aiops: alert %q escalated to cloud target %q (%s)", alertName, step.name, reason)
			} else {
				log.Printf("aiops: alert %q escalated to cloud (%s)", alertName, reason)
			}
			h.metrics.IncEscalationsTotal(step.display)
			return result, step, nil
		}
		h.metrics.IncEscalationFailuresTotal(step.display)
		if step.name != "" {
			log.Printf("aiops: cloud escalation to %q failed for alert %q (%s): %v", step.name, alertName, reason, err)
		} else {
			log.Printf("aiops: cloud escalation failed for alert %q (%s): %v", alertName, reason, err)
		}
		label := step.name
		if label == "" {
			label = "cloud"
		}
		failures = append(failures, fmt.Sprintf("%s: %v", label, err))
		if i+1 < len(steps) {
			log.Printf("aiops: alert %q falling back to escalation target %q", alertName, steps[i+1].name)
		}
	}
	if len(failures) == 1 {
		return aiops.SummarizeResult{}, escalationStep{}, errors.New(failures[0])
	}
	return aiops.SummarizeResult{}, escalationStep{}, fmt.Errorf("all %d escalation targets failed: %s", len(failures), strings.Join(failures, "; "))
}

// buildLegacyEscalations turns the legacy cloud block plus
// cloud_fallbacks into an escalation chain. The cloud block's own step
// has an empty name so its log lines stay the legacy ones; every step's
// display is its provider, which is what a legacy deployment's
// notifications can meaningfully show (there are no target names).
func buildLegacyEscalations(cfg *config.Config, cloud model.LLM) ([]escalationStep, error) {
	if cloud == nil {
		return nil, nil
	}
	steps := []escalationStep{{display: providerName(cfg.Cloud), llm: cloud}}
	for i, fb := range cfg.CloudFallbacks {
		label := fmt.Sprintf("cloud_fallbacks[%d]", i)
		c, err := buildCloud(label, fb)
		if err != nil {
			return nil, err
		}
		steps = append(steps, escalationStep{name: label, display: providerName(fb) + " (" + label + ")", llm: c})
	}
	return steps, nil
}

// providerName is a cloud block's provider with the "" → "gemini" default
// spelled out.
func providerName(c *config.CloudConfig) string {
	if c == nil || c.Provider == "" {
		return "gemini"
	}
	return c.Provider
}

// buildSummarizer wraps summarizer plus summarizer.fallbacks. Each
// backend gets its resolved health-check settings (defaults applied, a
// fallback inheriting whatever the top-level block set); with the checks
// turned off and no fallbacks, a legacy config's behavior (and error
// text) is exactly aiops.NewSummarizer's.
func buildSummarizer(c config.LLMConfig) *aiops.Summarizer {
	backend := func(name string, lc config.LLMConfig, h config.SummarizerHealth) aiops.SummarizerBackend {
		return aiops.SummarizerBackend{
			Name: name,
			LLM: model.NewOpenAIClient(model.OpenAIClientConfig{
				Endpoint: lc.Endpoint,
				Model:    lc.Model,
				Backend:  "aiops-summarizer",
				APIKey:   lc.APIKey,
				Timeout:  time.Duration(lc.TimeoutSec) * time.Second,
			}),
			ProbeTimeout:    time.Duration(h.ProbeTimeoutSec) * time.Second,
			BreakerFailures: h.BreakerFailures,
			BreakerCooldown: time.Duration(h.BreakerCooldownSec) * time.Second,
		}
	}
	top := c.Health(config.DefaultSummarizerHealth)
	backends := []aiops.SummarizerBackend{backend("summarizer", c, top)}
	for i, fb := range c.Fallbacks {
		backends = append(backends, backend(fmt.Sprintf("summarizer.fallbacks[%d]", i), fb, fb.Health(top)))
	}
	return aiops.NewSummarizerWithFallbacks(backends)
}

// startupNotes returns the extra lines printed under the startup banner:
// fallback counts when fallbacks are configured, and warnings about
// settings that parse fine but won't do what they look like they do. A
// config with none of these gets no extra lines, so a legacy
// deployment's banner is unchanged.
func startupNotes(cfg *config.Config) []string {
	var notes []string
	if n := len(cfg.Summarizer.Fallbacks); n > 0 {
		eps := make([]string, n)
		for i, fb := range cfg.Summarizer.Fallbacks {
			eps[i] = fmt.Sprintf("%s (%s)", fb.Endpoint, fb.Model)
		}
		notes = append(notes, "summarizer fallbacks: "+strings.Join(eps, ", "))
	}
	if n := len(cfg.CloudFallbacks); n > 0 {
		ps := make([]string, n)
		for i, fb := range cfg.CloudFallbacks {
			ps[i] = providerName(fb)
		}
		notes = append(notes, "cloud fallbacks: "+strings.Join(ps, ", "))
	}
	if len(cfg.Escalation.AlwaysCloud) > 0 {
		if missing := routesWithoutEscalation(cfg); len(missing) > 0 {
			notes = append(notes, fmt.Sprintf("⚠️  escalation.always_cloud is set, but route(s) %s have no escalation target — always_cloud alerts routed there stay on the local result",
				strings.Join(missing, ", ")))
		}
	}
	notes = append(notes, pollBeyondGraceNotes(cfg)...)
	return notes
}

// defaultShutdownGrace is shutdown_grace_sec's default.
const defaultShutdownGrace = 5 * time.Minute

// shutdownGrace is how long a SIGTERM'd process waits for in-flight
// analyses: shutdown_grace_sec, or defaultShutdownGrace when unset.
func shutdownGrace(cfg *config.Config) time.Duration {
	if cfg.ShutdownGraceSec > 0 {
		return time.Duration(cfg.ShutdownGraceSec) * time.Second
	}
	return defaultShutdownGrace
}

// pollBeyondGraceNotes warns about each investigation-style target
// (gcp-cloud-assist, aws-devops-agent) that may wait longer for its
// result than a restart waits for in-flight analyses, so a redeploy can
// cut an escalation to it short. Every place a target can be configured
// is checked: the legacy cloud block, cloud_fallbacks and
// escalation_targets.
func pollBeyondGraceNotes(cfg *config.Config) []string {
	grace := shutdownGrace(cfg)
	var notes []string
	check := func(label string, c *config.CloudConfig) {
		if c == nil {
			return
		}
		var limit time.Duration
		switch c.Provider {
		case "gcp-cloud-assist":
			limit = time.Duration(c.PollTimeoutSec) * time.Second
			if limit <= 0 {
				limit = model.CloudAssistDefaultPollTimeout
			}
		case "aws-devops-agent":
			limit = model.DevOpsAgentDefaultPollTimeout
		default:
			return
		}
		if limit > grace {
			notes = append(notes, fmt.Sprintf("⚠️  %s (%s) can wait up to %s for an investigation, longer than shutdown_grace_sec (%s) — a restart during one cuts the escalation short",
				label, c.Provider, limit, grace))
		}
	}
	check("cloud", cfg.Cloud)
	for i, fb := range cfg.CloudFallbacks {
		check(fmt.Sprintf("cloud_fallbacks[%d]", i), fb)
	}
	names := make([]string, 0, len(cfg.EscalationTargets))
	for name := range cfg.EscalationTargets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		check("escalation_targets."+name, cfg.EscalationTargets[name])
	}
	return notes
}
