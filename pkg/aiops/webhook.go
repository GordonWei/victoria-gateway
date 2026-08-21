package aiops

import (
	"encoding/json"
	"fmt"
)

// ParseWebhook decodes an Alertmanager webhook JSON body into a
// WebhookPayload. It validates the payload version (must be "4") and
// requires at least one alert entry — an empty alerts slice means either
// Alertmanager misconfigured or something upstream is broken, and we'd
// rather fail loudly than silently do nothing.
func ParseWebhook(body []byte) (*WebhookPayload, error) {
	var p WebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("webhook: unmarshal: %w", err)
	}

	if p.Version != "4" {
		return nil, fmt.Errorf("webhook: unsupported payload version %q (expected \"4\")", p.Version)
	}
	if len(p.Alerts) == 0 {
		return nil, fmt.Errorf("webhook: payload contains zero alerts")
	}

	return &p, nil
}
