package metrics

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The files under deploy/ that query /metrics are tested here against
// what this package actually exposes, so a renamed or removed metric
// breaks the build instead of leaving an empty panel or a rule that can
// never fire. promtool is not a dependency of this module: the rules file
// is checked for structure, known metric names and balanced brackets, not
// parsed as PromQL. Run `promtool check rules deploy/prometheus-rules.yaml`
// where promtool is available for the full check.

const (
	dashboardPath = "../../deploy/grafana-dashboard-metrics.json"
	rulesPath     = "../../deploy/prometheus-rules.yaml"
)

var metricName = regexp.MustCompile(`victoria_gateway_[a-z0-9_]+`)

// exposedNames scrapes a Counters with every family populated (several
// only appear once they have a labeled sample) and returns the set of
// metric names in the output.
func exposedNames(t *testing.T) map[string]bool {
	t.Helper()
	c := &Counters{}
	c.SetBuildInfo("v0", "abc")
	c.IncAlertsTotal("r")
	c.IncNotifyPush("ch", false)
	c.IncNotifyPush("ch", true)
	c.SetLocalLLMBreakerOpen("summarizer", true)
	c.IncLocalLLMSkippedTotal("summarizer", "probe")
	c.ObserveCloudLLMDuration("t", time.Second)
	c.ObserveLokiQueryDuration("loki", time.Second)
	names := map[string]bool{}
	for _, line := range strings.Split(scrapeBody(c), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		names[metricName.FindString(line)] = true
	}
	if len(names) < 30 {
		t.Fatalf("only %d metric names scraped; the fixture is missing families", len(names))
	}
	return names
}

func checkNames(t *testing.T, where, expr string, exposed map[string]bool) int {
	t.Helper()
	found := metricName.FindAllString(expr, -1)
	for _, n := range found {
		if !exposed[n] {
			t.Errorf("%s references %s, which pkg/metrics does not expose", where, n)
		}
	}
	return len(found)
}

func checkBalanced(t *testing.T, where, expr string) {
	t.Helper()
	pairs := map[rune]rune{')': '(', ']': '[', '}': '{'}
	var stack []rune
	inQuote := false
	for _, r := range expr {
		switch {
		case r == '"':
			inQuote = !inQuote
		case inQuote:
		case r == '(' || r == '[' || r == '{':
			stack = append(stack, r)
		case pairs[r] != 0:
			if len(stack) == 0 || stack[len(stack)-1] != pairs[r] {
				t.Errorf("%s: unbalanced %q in %s", where, r, expr)
				return
			}
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) != 0 || inQuote {
		t.Errorf("%s: unclosed bracket or quote in %s", where, expr)
	}
}

func TestDeployDashboard_ReferencesOnlyExposedMetrics(t *testing.T) {
	raw, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatal(err)
	}
	var dash struct {
		UID    string `json:"uid"`
		Panels []struct {
			ID         int    `json:"id"`
			Title      string `json:"title"`
			Datasource struct {
				Type string `json:"type"`
				UID  string `json:"uid"`
			} `json:"datasource"`
			GridPos struct{ X, Y, W, H int } `json:"gridPos"`
			Targets []struct {
				RefID string `json:"refId"`
				Expr  string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&dash); err != nil {
		t.Fatalf("dashboard is not valid JSON: %v", err)
	}
	if dash.UID == "" || len(dash.Panels) == 0 {
		t.Fatal("dashboard has no uid or no panels")
	}
	exposed := exposedNames(t)
	ids := map[int]bool{}
	refs := 0
	for _, p := range dash.Panels {
		where := "panel " + p.Title
		if ids[p.ID] {
			t.Errorf("duplicate panel id %d", p.ID)
		}
		ids[p.ID] = true
		if p.Datasource.Type != "prometheus" || p.Datasource.UID != "victoria-gateway-prometheus" {
			t.Errorf("%s: datasource %+v", where, p.Datasource)
		}
		if p.GridPos.W <= 0 || p.GridPos.X+p.GridPos.W > 24 || p.GridPos.H <= 0 {
			t.Errorf("%s: gridPos %+v outside the 24-column grid", where, p.GridPos)
		}
		if len(p.Targets) == 0 {
			t.Errorf("%s has no targets", where)
		}
		for _, tg := range p.Targets {
			if tg.Expr == "" || tg.RefID == "" {
				t.Errorf("%s: target without expr or refId", where)
			}
			refs += checkNames(t, where, tg.Expr, exposed)
			checkBalanced(t, where, tg.Expr)
		}
	}
	// Every exposed family should be on the dashboard somewhere.
	all := string(raw)
	for n := range exposed {
		base := strings.TrimSuffix(strings.TrimSuffix(n, "_sum"), "_count")
		if !strings.Contains(all, base) {
			t.Errorf("metric %s is not on the dashboard", n)
		}
	}
	if refs == 0 {
		t.Fatal("no metric references found")
	}
}

type ruleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert       string            `yaml:"alert"`
			Expr        string            `yaml:"expr"`
			For         string            `yaml:"for"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

var alertNameShape = regexp.MustCompile(`^VictoriaGateway[A-Z][A-Za-z]+$`)

func TestDeployRules_StructureAndMetricNames(t *testing.T) {
	raw, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatal(err)
	}
	var rf ruleFile
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // a misspelled key (anotations:) fails here
	if err := dec.Decode(&rf); err != nil {
		t.Fatalf("rules file does not parse as a Prometheus rule file: %v", err)
	}
	if len(rf.Groups) == 0 {
		t.Fatal("no rule groups")
	}
	exposed := exposedNames(t)
	seen := map[string]bool{}
	for _, g := range rf.Groups {
		if g.Name == "" || len(g.Rules) == 0 {
			t.Errorf("group %q is unnamed or empty", g.Name)
		}
		for _, r := range g.Rules {
			where := "rule " + r.Alert
			if !alertNameShape.MatchString(r.Alert) {
				t.Errorf("%s: alert name should look like VictoriaGatewaySomething", where)
			}
			if seen[r.Alert] {
				t.Errorf("duplicate alert %s", r.Alert)
			}
			seen[r.Alert] = true
			if strings.TrimSpace(r.Expr) == "" {
				t.Errorf("%s: empty expr", where)
			}
			if !strings.Contains(r.Expr, `job="victoria-gateway"`) {
				t.Errorf("%s: expr does not select job=\"victoria-gateway\"", where)
			}
			checkNames(t, where, r.Expr, exposed)
			checkBalanced(t, where, r.Expr)
			if r.For != "" {
				if _, err := time.ParseDuration(r.For); err != nil {
					t.Errorf("%s: for %q: %v", where, r.For, err)
				}
			}
			switch r.Labels["severity"] {
			case "critical", "warning", "info":
			default:
				t.Errorf("%s: severity %q", where, r.Labels["severity"])
			}
			if r.Annotations["summary"] == "" || r.Annotations["description"] == "" {
				t.Errorf("%s: needs summary and description annotations", where)
			}
			for k, v := range r.Annotations {
				if strings.Count(v, "{{") != strings.Count(v, "}}") {
					t.Errorf("%s: annotation %s has unbalanced template braces", where, k)
				}
			}
		}
	}
	for _, want := range []string{"VictoriaGatewayDown", "VictoriaGatewayVersionChanged", "VictoriaGatewayEscalationFailureRate", "VictoriaGatewayLocalLLMSlow", "VictoriaGatewayNotificationFailures"} {
		if !seen[want] {
			t.Errorf("missing rule %s", want)
		}
	}
}
