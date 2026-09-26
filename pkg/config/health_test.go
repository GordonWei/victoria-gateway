package config

import "testing"

func intp(v int) *int { return &v }

func TestLoad_SummarizerHealthDefaultsAndInheritance(t *testing.T) {
	c := loadYAML(t, `
summarizer:
  endpoint: "http://mlx:8091"
  model: primary
  fallbacks:
    - {endpoint: "http://a:1234", model: a}
    - {endpoint: "http://b:1234", model: b, probe_timeout_sec: 0}
loki: {endpoint: "http://loki:3100"}
`)
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	top := c.Summarizer.Health(DefaultSummarizerHealth)
	if top != DefaultSummarizerHealth {
		t.Errorf("unwritten top-level health = %+v, want defaults %+v", top, DefaultSummarizerHealth)
	}
	if got := c.Summarizer.Fallbacks[0].Health(top); got != top {
		t.Errorf("fallbacks[0] health = %+v, want inherited %+v", got, top)
	}
	if got := c.Summarizer.Fallbacks[1].Health(top); got.ProbeTimeoutSec != 0 {
		t.Errorf("fallbacks[1] probe_timeout_sec = %d, want the explicit 0 kept", got.ProbeTimeoutSec)
	}
}

func TestLoad_SummarizerHealthTopLevelOverrideIsInherited(t *testing.T) {
	c := loadYAML(t, `
summarizer:
  endpoint: "http://mlx:8091"
  probe_timeout_sec: 0
  fallbacks:
    - {endpoint: "http://a:1234", probe_timeout_sec: 3}
    - {endpoint: "http://b:1234"}
loki: {endpoint: "http://loki:3100"}
`)
	top := c.Summarizer.Health(DefaultSummarizerHealth)
	if top.ProbeTimeoutSec != 0 {
		t.Errorf("top probe_timeout_sec = %d, want 0", top.ProbeTimeoutSec)
	}
	if got := c.Summarizer.Fallbacks[0].Health(top).ProbeTimeoutSec; got != 3 {
		t.Errorf("fallbacks[0] probe_timeout_sec = %d, want its own 3", got)
	}
	if got := c.Summarizer.Fallbacks[1].Health(top).ProbeTimeoutSec; got != 0 {
		t.Errorf("fallbacks[1] probe_timeout_sec = %d, want the top-level 0 inherited", got)
	}
}

func TestValidate_SummarizerHealthRejectsNegative(t *testing.T) {
	c := validConfig()
	c.Summarizer.ProbeTimeoutSec = intp(-1)
	wantErrContaining(t, c.Validate(), "summarizer.probe_timeout_sec must be >= 0")

	c = validConfig()
	c.Summarizer.Fallbacks = []LLMConfig{{Endpoint: "http://b", ProbeTimeoutSec: intp(-1)}}
	wantErrContaining(t, c.Validate(), "summarizer.fallbacks[0].probe_timeout_sec must be >= 0")
}
