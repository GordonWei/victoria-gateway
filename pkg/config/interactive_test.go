package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ragOnConfig() *Config {
	c := validConfig()
	c.RAG = &RAGConfig{Enabled: true, PostgresDSN: "postgres://u@db/x", EmbeddingEndpoint: "http://emb", EmbeddingModel: "m"}
	return c
}

const testToken32 = "0123456789abcdef0123456789abcdef"

func TestMCP_DefaultsOffAndUnchanged(t *testing.T) {
	c := validConfig()
	if c.MCPEnabled() {
		t.Fatal("MCPEnabled with no mcp block")
	}
	c.MCP = &MCPConfig{} // present but not enabled: nothing is validated
	if err := c.Validate(); err != nil {
		t.Fatalf("disabled mcp block: %v", err)
	}
	var nilCfg *MCPConfig
	if nilCfg.EffectiveMaxResults() != MCPDefaultMaxResults || nilCfg.EffectiveMaxOutputBytes() != MCPDefaultMaxOutputBytes {
		t.Fatal("defaults on a nil MCPConfig")
	}
}

func TestMCP_Validate(t *testing.T) {
	c := validConfig()
	c.MCP = &MCPConfig{Enabled: true}
	wantErrContaining(t, c.Validate(), "rag.enabled is not")

	c = ragOnConfig()
	off := false
	c.RAG.AuditLog = &off
	c.MCP = &MCPConfig{Enabled: true}
	wantErrContaining(t, c.Validate(), "audit_log is false")

	c = ragOnConfig()
	c.MCP = &MCPConfig{Enabled: true}
	if err := c.Validate(); err != nil {
		t.Fatalf("stdio-only mcp: %v", err)
	}

	for _, tc := range []struct {
		m    MCPConfig
		want string
	}{
		{MCPConfig{Enabled: true, MaxResults: -1}, "max_results"},
		{MCPConfig{Enabled: true, MaxResults: 21}, "max_results"},
		{MCPConfig{Enabled: true, MaxOutputBytes: 100}, "max_output_bytes"},
		{MCPConfig{Enabled: true, MaxOutputBytes: 1 << 20}, "max_output_bytes"},
		{MCPConfig{Enabled: true, HTTP: &MCPHTTPConfig{BearerToken: testToken32}}, "listen_addr is empty"},
		{MCPConfig{Enabled: true, HTTP: &MCPHTTPConfig{ListenAddr: ":9300", BearerToken: "short"}}, "bearer_token of at least 32"},
		{MCPConfig{Enabled: true, HTTP: &MCPHTTPConfig{ListenAddr: ":8090", BearerToken: testToken32}}, "same address"},
	} {
		c := ragOnConfig()
		m := tc.m
		c.MCP = &m
		wantErrContaining(t, c.Validate(), tc.want)
	}

	c = ragOnConfig()
	c.MCP = &MCPConfig{Enabled: true, MaxResults: 20, MaxOutputBytes: 2048, HTTP: &MCPHTTPConfig{ListenAddr: "127.0.0.1:9300", BearerToken: testToken32}}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid http mcp: %v", err)
	}
	if c.MCP.EffectiveMaxResults() != 20 || c.MCP.EffectiveMaxOutputBytes() != 2048 {
		t.Fatal("explicit limits not returned")
	}
}

func validActionsConfig() *Config {
	c := ragOnConfig()
	c.Telegram = TelegramConfig{BotToken: "t", ChatID: -100}
	c.TelegramActions = &TelegramActionsConfig{Enabled: true, AllowedUserIDs: []int64{42}, HMACSecret: testToken32}
	return c
}

func TestTelegramActions_Validate(t *testing.T) {
	c := validConfig()
	c.TelegramActions = &TelegramActionsConfig{} // disabled: ignored
	if err := c.Validate(); err != nil || c.TelegramActionsEnabled() {
		t.Fatalf("disabled block: err=%v enabled=%v", err, c.TelegramActionsEnabled())
	}
	if err := validActionsConfig().Validate(); err != nil {
		t.Fatalf("valid: %v", err)
	}
	ta := validActionsConfig().TelegramActions
	if ta.EffectiveButtonTTLSec() != 3600 || ta.EffectiveSilenceDurationSec() != 3600 || ta.EffectivePollTimeoutSec() != 30 {
		t.Fatal("defaults")
	}

	cases := []struct {
		mut  func(c *Config)
		want string
	}{
		{func(c *Config) { c.Telegram.BotToken = "" }, "no bot_token/chat_id"},
		{func(c *Config) { c.Telegram.ChatID = 0 }, "no bot_token/chat_id"},
		{func(c *Config) { c.RAG = nil }, "audit log is off"},
		{func(c *Config) { off := false; c.RAG.AuditLog = &off }, "audit log is off"},
		{func(c *Config) { c.TelegramActions.AllowedUserIDs = nil }, "allowed_user_ids is empty"},
		{func(c *Config) { c.TelegramActions.AllowedUserIDs = []int64{-5} }, "positive"},
		{func(c *Config) { c.TelegramActions.HMACSecret = "short" }, "hmac_secret"},
		{func(c *Config) { c.TelegramActions.ButtonTTLSec = 10 }, "button_ttl_sec"},
		{func(c *Config) { c.TelegramActions.ButtonTTLSec = 90000 }, "button_ttl_sec"},
		{func(c *Config) { c.TelegramActions.SilenceDurationSec = 7201 }, "two hours"},
		{func(c *Config) { c.TelegramActions.SilenceDurationSec = 5 }, "silence_duration_sec"},
		{func(c *Config) { c.TelegramActions.PollTimeoutSec = 51 }, "poll_timeout_sec"},
		{func(c *Config) { c.TelegramActions.PollTimeoutSec = -1 }, "poll_timeout_sec"},
	}
	for _, tc := range cases {
		c := validActionsConfig()
		tc.mut(c)
		wantErrContaining(t, c.Validate(), tc.want)
	}
	c = validActionsConfig()
	c.TelegramActions.SilenceDurationSec = 7200
	if err := c.Validate(); err != nil {
		t.Fatalf("two-hour silence must be allowed: %v", err)
	}
}

func TestInteractiveEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	yaml := strings.Join([]string{
		"mcp:", "  enabled: true", "  http:", "    listen_addr: \":9300\"",
		"telegram_actions:", "  enabled: true",
	}, "\n")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VICTORIA_GATEWAY_MCP_BEARER_TOKEN", "from-env-token")
	t.Setenv("VICTORIA_GATEWAY_TELEGRAM_ACTIONS_HMAC_SECRET", "from-env-secret")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.MCP.HTTP.BearerToken != "from-env-token" || c.TelegramActions.HMACSecret != "from-env-secret" {
		t.Fatalf("env overrides not applied: %+v %+v", c.MCP.HTTP, c.TelegramActions)
	}

	// No block, no effect: the env var must not create one.
	p2 := filepath.Join(dir, "c2.yaml")
	if err := os.WriteFile(p2, []byte("listen_addr: \":8090\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p2)
	if err != nil {
		t.Fatal(err)
	}
	if c2.MCP != nil || c2.TelegramActions != nil {
		t.Fatal("env var created a config block")
	}
}
