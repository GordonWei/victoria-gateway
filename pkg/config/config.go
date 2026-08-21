// Package config loads orch-gateway's YAML config. This service does one
// thing: receive Alertmanager webhooks, pull the surrounding Loki logs, ask
// an OpenAI-compatible LLM endpoint to summarize what happened, and push
// that summary to Telegram. The config shape is flat and matches that.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ListenAddr string         `yaml:"listen_addr"` // e.g. ":8090"
	Loki       LokiConfig     `yaml:"loki"`
	Summarizer LLMConfig      `yaml:"summarizer"`
	Telegram   TelegramConfig `yaml:"telegram"`
}

// LokiConfig points at the Loki instance to query for context around a
// fired alert.
type LokiConfig struct {
	Endpoint    string `yaml:"endpoint"`     // e.g. "http://loki:3100"
	LookbackSec int    `yaml:"lookback_sec"` // how far before the alert's startsAt to begin the query window
	Limit       int    `yaml:"limit"`        // max log lines fetched per query
}

// LLMConfig is the OpenAI-compatible endpoint the summarizer calls (LM
// Studio, Ollama, vLLM, etc. — anything that speaks /v1/chat/completions).
type LLMConfig struct {
	Endpoint string `yaml:"endpoint"`
	Model    string `yaml:"model"`
}

// TelegramConfig, if BotToken is set, makes the webhook handler push each
// alert's LLM summary to this chat via the Telegram Bot API. Without it,
// the summary is still computed but only ever ends up in the webhook's
// HTTP response body, which Alertmanager discards (it only checks the
// status code).
type TelegramConfig struct {
	BotToken string `yaml:"bot_token"`
	ChatID   int64  `yaml:"chat_id"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg := &Config{
		ListenAddr: ":8090",
		Loki:       LokiConfig{LookbackSec: 300, Limit: 200},
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}
