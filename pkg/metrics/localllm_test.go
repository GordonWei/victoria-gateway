package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func scrapeBody(c *Counters) string {
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestCounters_LocalLLMHealthFamilies(t *testing.T) {
	c := &Counters{}
	c.SetLocalLLMBreakerOpen("summarizer", false)
	c.SetLocalLLMBreakerOpen("summarizer.fallbacks[0]", true)
	c.IncLocalLLMSkippedTotal("summarizer", "probe")
	c.IncLocalLLMSkippedTotal("summarizer", "probe")
	c.IncLocalLLMSkippedTotal("summarizer", "breaker")
	c.IncLocalLLMSkippedTotal("summarizer.fallbacks[0]", "breaker")

	body := scrapeBody(c)
	for _, want := range []string{
		"# TYPE victoria_gateway_local_llm_breaker_open gauge\n",
		`victoria_gateway_local_llm_breaker_open{backend="summarizer"} 0`,
		`victoria_gateway_local_llm_breaker_open{backend="summarizer.fallbacks[0]"} 1`,
		"# TYPE victoria_gateway_local_llm_skipped_total counter\n",
		`victoria_gateway_local_llm_skipped_total{backend="summarizer",reason="breaker"} 1`,
		`victoria_gateway_local_llm_skipped_total{backend="summarizer",reason="probe"} 2`,
		`victoria_gateway_local_llm_skipped_total{backend="summarizer.fallbacks[0]",reason="breaker"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q; got:\n%s", want, body)
		}
	}
	if strings.Contains(body, "victoria_gateway_local_llm_skipped_total 0") {
		t.Error("unlabeled 0 sample kept alongside labeled ones")
	}

	// Breaker closing flips the gauge back.
	c.SetLocalLLMBreakerOpen("summarizer.fallbacks[0]", false)
	if body := scrapeBody(c); !strings.Contains(body, `victoria_gateway_local_llm_breaker_open{backend="summarizer.fallbacks[0]"} 0`) {
		t.Errorf("gauge not reset to 0; got:\n%s", body)
	}
}

func TestCounters_LocalLLMHealthEmptyAndNil(t *testing.T) {
	var nilCounters *Counters
	nilCounters.SetLocalLLMBreakerOpen("x", true) // no-ops, no panic
	nilCounters.IncLocalLLMSkippedTotal("x", "probe")

	for name, c := range map[string]*Counters{"empty": {}, "nil": nil} {
		body := scrapeBody(c)
		if !strings.Contains(body, "# TYPE victoria_gateway_local_llm_breaker_open gauge\n") {
			t.Errorf("%s: gauge family not declared; got:\n%s", name, body)
		}
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "victoria_gateway_local_llm_breaker_open") {
				t.Errorf("%s: gauge sample %q with no backend registered", name, line)
			}
		}
		if !strings.Contains(body, "victoria_gateway_local_llm_skipped_total 0\n") {
			t.Errorf("%s: want the skipped counter as an unlabeled 0; got:\n%s", name, body)
		}
	}
}

// Every family is declared exactly once, however many series it has.
func TestCounters_OneHelpAndTypePerFamily(t *testing.T) {
	c := &Counters{}
	c.SetLocalLLMBreakerOpen("a", true)
	c.SetLocalLLMBreakerOpen("b", false)
	c.IncLocalLLMSkippedTotal("a", "probe")
	c.IncLocalLLMSkippedTotal("b", "breaker")
	c.IncAlertsTotal("r1")
	c.IncAlertsTotal("r2")
	seen := map[string]int{}
	for _, line := range strings.Split(scrapeBody(c), "\n") {
		if strings.HasPrefix(line, "# HELP ") || strings.HasPrefix(line, "# TYPE ") {
			seen[strings.Join(strings.Fields(line)[:3], " ")]++
		}
	}
	for decl, n := range seen {
		if n != 1 {
			t.Errorf("%q appears %d times", decl, n)
		}
	}
	for _, want := range []string{
		"# HELP victoria_gateway_local_llm_breaker_open", "# TYPE victoria_gateway_local_llm_breaker_open",
		"# HELP victoria_gateway_local_llm_skipped_total", "# TYPE victoria_gateway_local_llm_skipped_total",
	} {
		if seen[want] != 1 {
			t.Errorf("%q declared %d times, want 1", want, seen[want])
		}
	}
}

func TestCounters_LocalLLMLabelsEscaped(t *testing.T) {
	c := &Counters{}
	c.SetLocalLLMBreakerOpen("a\"b\\c\nd", true)
	c.IncLocalLLMSkippedTotal("a\"b", "pro\nbe")
	body := scrapeBody(c)
	for _, want := range []string{
		`victoria_gateway_local_llm_breaker_open{backend="a\"b\\c\nd"} 1`,
		`victoria_gateway_local_llm_skipped_total{backend="a\"b",reason="pro\nbe"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q; got:\n%s", want, body)
		}
	}
}
