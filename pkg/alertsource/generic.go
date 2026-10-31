package alertsource

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/config"
)

// Generic parses one configured generic source. Unlike ParseGrafana it
// rejects the whole request when an alert can't be built — a missing
// alertname, an unmapped status value, an unreadable start time, or no
// host/Kubernetes identity after mapping — because those mean the
// mapping and the sender disagree, and an alert with an empty host would
// otherwise reach log lookup and quietly find nothing.
type Generic struct {
	Cfg config.GenericSourceConfig
	// Now is when the webhook arrived; used for starts_at when no path is
	// mapped. Nil means time.Now.
	Now func() time.Time
}

// Parse converts body into the gateway's payload.
func (g Generic) Parse(body []byte) (*aiops.WebhookPayload, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("generic source %q: unmarshal: %w", g.Cfg.Name, err)
	}

	items := []any{root}
	if g.Cfg.AlertsPath != "" {
		v, ok := lookup(root, g.Cfg.AlertsPath)
		arr, isArr := v.([]any)
		if !ok || !isArr {
			return nil, fmt.Errorf("generic source %q: alerts_path %q is not an array in the body", g.Cfg.Name, g.Cfg.AlertsPath)
		}
		items = arr
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("generic source %q: payload contains zero alerts", g.Cfg.Name)
	}

	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	out := &aiops.WebhookPayload{Version: "generic/" + g.Cfg.Name, Receiver: g.Cfg.Name, Status: "firing"}
	anyFiring := false
	for i, item := range items {
		a, err := g.alert(item, now)
		if err != nil {
			return nil, fmt.Errorf("generic source %q: alerts[%d]: %w", g.Cfg.Name, i, err)
		}
		if a.Status == "firing" {
			anyFiring = true
		}
		out.Alerts = append(out.Alerts, a)
	}
	if !anyFiring {
		out.Status = "resolved"
	}
	return out, nil
}

func (g Generic) alert(item any, now func() time.Time) (aiops.Alert, error) {
	f := g.Cfg.Fields
	name, ok := lookupString(item, f.AlertName)
	if !ok || name == "" {
		return aiops.Alert{}, fmt.Errorf("alertname: nothing at %q", f.AlertName)
	}

	labels := map[string]string{}
	for k, v := range g.Cfg.StaticLabels {
		labels[k] = v
	}
	for k, path := range g.Cfg.Labels {
		if v, ok := lookupString(item, path); ok && v != "" {
			labels[k] = v
		}
	}
	labels["alertname"] = name

	ann := map[string]string{}
	for k, path := range g.Cfg.Annotations {
		if v, ok := lookupString(item, path); ok && v != "" {
			ann[k] = v
		}
	}

	status := "firing"
	if f.Status != "" {
		raw, ok := lookupString(item, f.Status)
		if !ok {
			return aiops.Alert{}, fmt.Errorf("status: nothing at %q", f.Status)
		}
		switch mapped, inMap := g.Cfg.StatusMap[raw]; {
		case inMap:
			status = mapped
		case raw == "firing" || raw == "resolved":
			status = raw
		default:
			return aiops.Alert{}, fmt.Errorf("status: value %q is not in status_map and is not firing/resolved", raw)
		}
	}

	startsAt := now().UTC()
	if f.StartsAt != "" {
		raw, ok := lookupString(item, f.StartsAt)
		if !ok || raw == "" {
			return aiops.Alert{}, fmt.Errorf("starts_at: nothing at %q", f.StartsAt)
		}
		t, err := parseTime(raw, g.Cfg.StartsAtFormat)
		if err != nil {
			return aiops.Alert{}, fmt.Errorf("starts_at: %w", err)
		}
		startsAt = t
	}

	// The dedup table is shared by every alert input, and sources such as Zabbix
	// or Datadog number their events with short integers, so the source name is
	// part of the fingerprint: event 48213 from two sources is two alerts.
	fp := ""
	if f.Fingerprint != "" {
		v, ok := lookupString(item, f.Fingerprint)
		if !ok || v == "" {
			return aiops.Alert{}, fmt.Errorf("fingerprint: nothing at %q", f.Fingerprint)
		}
		fp = v
	} else {
		fp = labelFingerprint(labels)
	}
	fp = "generic:" + g.Cfg.Name + ":" + fp

	a := aiops.Alert{
		Status:      status,
		Labels:      labels,
		Annotations: ann,
		StartsAt:    startsAt.Format(time.RFC3339),
		EndsAt:      "0001-01-01T00:00:00Z",
		Fingerprint: fp,
	}
	if _, _, ok := a.AffectedIdentity(); !ok {
		return aiops.Alert{}, fmt.Errorf("alert %q has no host, instance, or namespace+pod/deployment/statefulset label after mapping — map one under labels", name)
	}
	return a, nil
}

func parseTime(raw, format string) (time.Time, error) {
	switch format {
	case "", "rfc3339":
		return time.Parse(time.RFC3339, raw)
	case "unix", "unix_ms":
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			// JSON numbers can arrive as "1.7e9"; accept whole floats too.
			fl, ferr := strconv.ParseFloat(raw, 64)
			if ferr != nil {
				return time.Time{}, fmt.Errorf("%q is not a %s timestamp", raw, format)
			}
			n = int64(fl)
		}
		if format == "unix_ms" {
			return time.UnixMilli(n).UTC(), nil
		}
		return time.Unix(n, 0).UTC(), nil
	default:
		return time.Parse(format, raw)
	}
}

// labelFingerprint stands in for Alertmanager's fingerprint when the
// source has none: a stable hash of the sorted label set, so the same
// alert re-sent hits the gateway's dedup window instead of being
// analyzed twice.
func labelFingerprint(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		_, _ = h.Write([]byte(k + "\x00" + labels[k] + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// lookup walks a dot path ("a.b.0.c", optional leading "$.") through
// decoded JSON. Numeric segments index arrays.
func lookup(v any, path string) (any, bool) {
	path = strings.TrimPrefix(strings.TrimPrefix(path, "$"), ".")
	if path == "" {
		return v, true
	}
	for _, seg := range strings.Split(path, ".") {
		switch node := v.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return nil, false
			}
			v = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			v = node[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// lookupString is lookup for leaf values: strings as-is, numbers and
// booleans in their JSON spelling. Objects, arrays and null are "not
// found" — a label can't hold them.
func lookupString(v any, path string) (string, bool) {
	got, ok := lookup(v, path)
	if !ok {
		return "", false
	}
	switch x := got.(type) {
	case string:
		return x, true
	case json.Number:
		return x.String(), true
	case bool:
		return strconv.FormatBool(x), true
	default:
		return "", false
	}
}
