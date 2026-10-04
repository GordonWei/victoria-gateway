package config

import (
	"fmt"
	"os"
)

// This file holds the two opt-in features that let something other than
// Alertmanager reach into victoria-gateway: the read-only MCP server and
// the Telegram action buttons. Both are off unless their block says
// enabled: true, and both refuse to start without the credential that
// keeps them closed to everyone else.

// MCP limits. The defaults are what an agent gets without asking; the hard
// caps are what it can never exceed, whatever the config says.
const (
	MCPDefaultMaxResults     = 5
	MCPHardMaxResults        = 20
	MCPDefaultMaxOutputBytes = 16 << 10
	MCPMinOutputBytes        = 1 << 10
	MCPHardMaxOutputBytes    = 64 << 10
	// MCPMinBearerTokenLen is the shortest bearer token the HTTP transport
	// accepts: 32 characters, the length of 16 random bytes hex-encoded.
	MCPMinBearerTokenLen = 32
)

// MCPConfig turns on `victoria-gateway mcp`, a read-only Model Context
// Protocol server over the RAG store (see cmd/victoria-gateway/mcpserver.go).
// Nil or Enabled false: the subcommand refuses to run, and nothing else in
// the service changes.
type MCPConfig struct {
	Enabled bool `yaml:"enabled"`
	// MaxResults caps how many incidents one tool call returns (top_k for
	// search_incidents, limit for list_pending). 0 means 5; at most 20.
	MaxResults int `yaml:"max_results"`
	// MaxOutputBytes caps one tool result's size. 0 means 16 KiB; between
	// 1 KiB and 64 KiB.
	MaxOutputBytes int `yaml:"max_output_bytes"`
	// HTTP, if set, lets `victoria-gateway mcp --http` serve the same tools
	// over streamable HTTP at ListenAddr, behind a bearer token. Unset
	// (the default) means stdio only: the server is whatever process
	// launched it, and nothing listens on the network.
	HTTP *MCPHTTPConfig `yaml:"http"`
}

// MCPHTTPConfig is the optional HTTP transport for the MCP server.
type MCPHTTPConfig struct {
	ListenAddr string `yaml:"listen_addr"`
	// BearerToken is required (at least MCPMinBearerTokenLen characters);
	// every request must carry "Authorization: Bearer <token>". Can come
	// from VICTORIA_GATEWAY_MCP_BEARER_TOKEN.
	BearerToken string `yaml:"bearer_token"`
}

// MCPEnabled reports whether the mcp block is present and switched on.
func (c *Config) MCPEnabled() bool { return c.MCP != nil && c.MCP.Enabled }

// EffectiveMaxResults returns MaxResults with its default applied.
func (m *MCPConfig) EffectiveMaxResults() int {
	if m == nil || m.MaxResults == 0 {
		return MCPDefaultMaxResults
	}
	return m.MaxResults
}

// EffectiveMaxOutputBytes returns MaxOutputBytes with its default applied.
func (m *MCPConfig) EffectiveMaxOutputBytes() int {
	if m == nil || m.MaxOutputBytes == 0 {
		return MCPDefaultMaxOutputBytes
	}
	return m.MaxOutputBytes
}

func (c *Config) validateMCP() error {
	m := c.MCP
	if m == nil || !m.Enabled {
		return nil
	}
	if c.RAG == nil || !c.RAG.Enabled {
		return fmt.Errorf("mcp.enabled is true but rag.enabled is not — the MCP tools read the RAG store, so RAG must be enabled")
	}
	if !c.RAG.AuditEnabled() {
		return fmt.Errorf("mcp.enabled is true but rag.audit_log is false — every MCP tool call is audited, so leave audit_log on (its default) to use mcp")
	}
	if m.MaxResults < 0 || m.MaxResults > MCPHardMaxResults {
		return fmt.Errorf("mcp.max_results must be between 0 and %d (0 means %d)", MCPHardMaxResults, MCPDefaultMaxResults)
	}
	if m.MaxOutputBytes != 0 && (m.MaxOutputBytes < MCPMinOutputBytes || m.MaxOutputBytes > MCPHardMaxOutputBytes) {
		return fmt.Errorf("mcp.max_output_bytes must be 0 (16 KiB) or between %d and %d", MCPMinOutputBytes, MCPHardMaxOutputBytes)
	}
	if m.HTTP != nil {
		if m.HTTP.ListenAddr == "" {
			return fmt.Errorf("mcp.http is set but mcp.http.listen_addr is empty — remove the http block to use stdio only")
		}
		if len(m.HTTP.BearerToken) < MCPMinBearerTokenLen {
			return fmt.Errorf("mcp.http needs a bearer_token of at least %d characters (or VICTORIA_GATEWAY_MCP_BEARER_TOKEN) — the HTTP transport never runs unauthenticated", MCPMinBearerTokenLen)
		}
		if SameListenAddr(m.HTTP.ListenAddr, c.ListenAddr) {
			return fmt.Errorf("mcp.http.listen_addr %q is the same address as listen_addr", m.HTTP.ListenAddr)
		}
	}
	return nil
}

// Telegram action limits.
const (
	TelegramActionsDefaultButtonTTLSec = 3600
	TelegramActionsMaxButtonTTLSec     = 24 * 3600
	TelegramActionsDefaultSilenceSec   = 3600
	// TelegramActionsMaxSilenceSec is the longest silence a button can
	// create: two hours. Anything longer belongs in Alertmanager's own
	// config or a maintenance window, where a human writes it down.
	TelegramActionsMaxSilenceSec      = 2 * 3600
	TelegramActionsDefaultPollTimeout = 30
	TelegramActionsMaxPollTimeout     = 50
	// TelegramActionsMinSecretLen is the shortest hmac_secret accepted.
	TelegramActionsMinSecretLen = 32
)

// TelegramActionsConfig adds inline buttons (acknowledge, escalate now,
// silence) to the top-level telegram block's notifications and listens for
// presses by long-polling the Bot API's getUpdates — no inbound webhook is
// opened. Nil or Enabled false: notifications look exactly as before and
// no polling happens. See cmd/victoria-gateway/tgactions.go.
type TelegramActionsConfig struct {
	Enabled bool `yaml:"enabled"`
	// AllowedUserIDs lists the Telegram user ids whose presses are acted
	// on. Required and non-empty: being in the chat is not enough.
	AllowedUserIDs []int64 `yaml:"allowed_user_ids"`
	// HMACSecret signs every button's callback data. At least 32
	// characters; can come from VICTORIA_GATEWAY_TELEGRAM_ACTIONS_HMAC_SECRET.
	HMACSecret string `yaml:"hmac_secret"`
	// ButtonTTLSec is how long a notification's buttons stay usable. 0
	// means 3600; at most 86400.
	ButtonTTLSec int `yaml:"button_ttl_sec"`
	// SilenceDurationSec is how long the silence button silences the
	// alert in Alertmanager. 0 means 3600; at most 7200.
	SilenceDurationSec int `yaml:"silence_duration_sec"`
	// PollTimeoutSec is getUpdates' long-poll timeout. 0 means 30; at most 50.
	PollTimeoutSec int `yaml:"poll_timeout_sec"`
}

// TelegramActionsEnabled reports whether the telegram_actions block is
// present and switched on.
func (c *Config) TelegramActionsEnabled() bool {
	return c.TelegramActions != nil && c.TelegramActions.Enabled
}

func (t *TelegramActionsConfig) orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// EffectiveButtonTTLSec returns ButtonTTLSec with its default applied.
func (t *TelegramActionsConfig) EffectiveButtonTTLSec() int {
	return t.orDefault(t.ButtonTTLSec, TelegramActionsDefaultButtonTTLSec)
}

// EffectiveSilenceDurationSec returns SilenceDurationSec with its default applied.
func (t *TelegramActionsConfig) EffectiveSilenceDurationSec() int {
	return t.orDefault(t.SilenceDurationSec, TelegramActionsDefaultSilenceSec)
}

// EffectivePollTimeoutSec returns PollTimeoutSec with its default applied.
func (t *TelegramActionsConfig) EffectivePollTimeoutSec() int {
	return t.orDefault(t.PollTimeoutSec, TelegramActionsDefaultPollTimeout)
}

func (c *Config) validateTelegramActions() error {
	t := c.TelegramActions
	if t == nil || !t.Enabled {
		return nil
	}
	if c.Telegram.BotToken == "" || c.Telegram.ChatID == 0 {
		return fmt.Errorf("telegram_actions.enabled is true but the top-level telegram block has no bot_token/chat_id — buttons are only added to that chat")
	}
	if len(t.AllowedUserIDs) == 0 {
		return fmt.Errorf("telegram_actions.allowed_user_ids is empty — list the Telegram user ids allowed to press the buttons")
	}
	for _, id := range t.AllowedUserIDs {
		if id <= 0 {
			return fmt.Errorf("telegram_actions.allowed_user_ids contains %d — user ids are positive (a negative id is a group, not a person)", id)
		}
	}
	if len(t.HMACSecret) < TelegramActionsMinSecretLen {
		return fmt.Errorf("telegram_actions.hmac_secret must be at least %d characters (or VICTORIA_GATEWAY_TELEGRAM_ACTIONS_HMAC_SECRET)", TelegramActionsMinSecretLen)
	}
	if t.ButtonTTLSec != 0 && (t.ButtonTTLSec < 60 || t.ButtonTTLSec > TelegramActionsMaxButtonTTLSec) {
		return fmt.Errorf("telegram_actions.button_ttl_sec must be 0 (%d) or between 60 and %d", TelegramActionsDefaultButtonTTLSec, TelegramActionsMaxButtonTTLSec)
	}
	if t.SilenceDurationSec != 0 && (t.SilenceDurationSec < 60 || t.SilenceDurationSec > TelegramActionsMaxSilenceSec) {
		return fmt.Errorf("telegram_actions.silence_duration_sec must be 0 (%d) or between 60 and %d (two hours)", TelegramActionsDefaultSilenceSec, TelegramActionsMaxSilenceSec)
	}
	if t.PollTimeoutSec < 0 || t.PollTimeoutSec > TelegramActionsMaxPollTimeout {
		return fmt.Errorf("telegram_actions.poll_timeout_sec must be between 0 and %d (0 means %d)", TelegramActionsMaxPollTimeout, TelegramActionsDefaultPollTimeout)
	}
	return nil
}

// applyInteractiveEnvOverrides is applyEnvOverrides' part for this file's
// blocks. Like the rest, a variable only fills in a block that config.yaml
// already has.
func applyInteractiveEnvOverrides(cfg *Config) {
	if cfg.MCP != nil && cfg.MCP.HTTP != nil {
		if v := os.Getenv("VICTORIA_GATEWAY_MCP_BEARER_TOKEN"); v != "" {
			cfg.MCP.HTTP.BearerToken = v
		}
	}
	if cfg.TelegramActions != nil {
		if v := os.Getenv("VICTORIA_GATEWAY_TELEGRAM_ACTIONS_HMAC_SECRET"); v != "" {
			cfg.TelegramActions.HMACSecret = v
		}
	}
}
