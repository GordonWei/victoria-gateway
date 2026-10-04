package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/audit"
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
	result, step, err := h.tryEscalationSteps(steps, alert, logs, ragContext, reason)
	h.auditEscalation(alert, reason, step, err)
	return result, step, err
}

// tryEscalationSteps is runEscalation's loop over the route's steps.
func (h *handler) tryEscalationSteps(steps []escalationStep, alert aiops.Alert, logs []aiops.LogEntry, ragContext, reason string) (aiops.SummarizeResult, escalationStep, error) {
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
			h.recordMitigation(alertName, step, result.Mitigation)
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

// Audit actor for the two escalation actions below: they're decided by the
// alert pipeline itself, not by a person, so there is no user or IP to
// record.
const auditActorSystem = "system"

// auditEscalation records that an alert was handed to a cloud escalation
// target — the one automatic action that spends money and sends alert
// context to a third party — and how it ended. Called once per
// runEscalation, after the fallback chain has finished: step is the target
// that answered, or err says why none did. A no-op unless rag.audit_log is
// on, and never blocks the alert (see recordAudit).
func (h *handler) auditEscalation(alert aiops.Alert, reason string, step escalationStep, err error) {
	detail := fmt.Sprintf("reason=%q target=%q result=ok", reason, step.display)
	if err != nil {
		detail = fmt.Sprintf("reason=%q result=failed error=%q", reason, err.Error())
	}
	h.auditAlertAction("escalation.trigger", alert, detail)
}

// auditEscalationRateLimited records that escalation.max_per_hour stopped
// an escalation that would otherwise have happened — the spend guardrail
// doing its job, which is worth being able to find later when someone asks
// why an alert got the local answer only.
func (h *handler) auditEscalationRateLimited(alert aiops.Alert, reason string) {
	h.auditAlertAction("escalation.rate_limited", alert,
		fmt.Sprintf("reason=%q max_per_hour=%d", reason, h.escalation.MaxPerHour))
}

func (h *handler) auditAlertAction(action string, alert aiops.Alert, detail string) {
	host, _, _ := alert.AffectedIdentity()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.recordAudit(ctx, audit.Entry{
		Actor:  auditActorSystem,
		Action: action,
		Target: fmt.Sprintf("alertname=%s host=%s", alert.Labels["alertname"], host),
		Detail: detail,
	})
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

// securityWarnings returns startup warnings for default-open settings that
// become risky in combination. None of them changes behavior — every
// default stays as it was — they only make the exposure visible:
//
//   - a cloud escalation target with no webhook_auth: anyone who can reach
//     the webhook can make the service spend money (a forged alertname on
//     always_cloud, or annotations that coax the local model into
//     escalate: true);
//   - a cloud escalation target with escalation.max_per_hour at 0
//     (unlimited): nothing bounds that spend;
//   - a cloud escalation target with rag.mask_log_excerpt off: log lines
//     reach the cloud model as they are;
//   - RAG on with no webui_auth: /incidents and /pending show log
//     excerpts to anyone who can reach the port.
func securityWarnings(cfg *config.Config) []string {
	var w []string
	ragOn := cfg.RAG != nil && cfg.RAG.Enabled
	if cfg.Cloud != nil || len(cfg.EscalationTargets) > 0 {
		if cfg.WebhookAuth == nil {
			w = append(w, "⚠️  cloud escalation is configured but webhook_auth is not set — anyone who can reach /webhook/alertmanager can trigger paid cloud calls")
		}
		if cfg.Escalation.MaxPerHour == 0 {
			w = append(w, "⚠️  cloud escalation is configured with escalation.max_per_hour 0 (unlimited) — nothing caps how many alerts go to a paid model")
		}
		if !ragOn || !cfg.RAG.MaskLogExcerpt {
			w = append(w, "⚠️  cloud escalation is configured but rag.mask_log_excerpt is off — log lines are sent to the cloud model unmasked (masking needs rag.enabled)")
		}
	}
	if ragOn && cfg.WebUIAuth == nil {
		w = append(w, "⚠️  rag is enabled but webui_auth is not set — /incidents and /pending show stored log excerpts without authentication")
	}
	return w
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
			if da := c.DevOpsAgent; da != nil && da.MitigationPlan {
				mt := time.Duration(da.MitigationTimeoutSec) * time.Second
				if mt <= 0 {
					mt = model.DevOpsAgentDefaultMitigationTimeout
				}
				limit += mt
			}
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
