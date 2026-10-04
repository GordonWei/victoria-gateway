package aiops

import (
	"errors"
	"fmt"
	"testing"
)

func TestAffectedIdentity_NamespacePodPreferredOverInstance(t *testing.T) {
	// A kube-state-metrics-sourced alert (e.g. KubePodCrashLooping) carries
	// both namespace/pod AND an instance label — but that instance is
	// kube-state-metrics' own scrape address, not the affected pod's host.
	// namespace+pod must win.
	a := Alert{Labels: map[string]string{
		"alertname": "KubePodCrashLooping",
		"namespace": "gitea",
		"pod":       "gitea-585b7c9565-r2lc7",
		"instance":  "198.51.100.100:8080",
	}}
	display, selector, ok := a.AffectedIdentity()
	if !ok {
		t.Fatal("AffectedIdentity() ok=false, want true")
	}
	if display != "gitea/gitea-585b7c9565-r2lc7" {
		t.Errorf("display = %q, want %q", display, "gitea/gitea-585b7c9565-r2lc7")
	}
	if selector != `{namespace="gitea",pod="gitea-585b7c9565-r2lc7"}` {
		t.Errorf("selector = %q", selector)
	}
}

func TestAffectedIdentity_FallsBackToHost(t *testing.T) {
	a := Alert{Labels: map[string]string{
		"alertname": "InstanceDown",
		"instance":  "192.0.2.6:9222",
	}}
	display, selector, ok := a.AffectedIdentity()
	if !ok {
		t.Fatal("AffectedIdentity() ok=false, want true")
	}
	if display != "192.0.2.6:9222" {
		t.Errorf("display = %q, want %q", display, "192.0.2.6:9222")
	}
	if selector != `{host="192.0.2.6:9222"}` {
		t.Errorf("selector = %q", selector)
	}
}

func TestAffectedIdentity_DeploymentWorkloadUsesNamespaceSelector(t *testing.T) {
	// KubeDeploymentReplicasMismatch describes a Deployment object, not a
	// specific pod — kube-state-metrics never attaches a "pod" label to
	// it. Falling back to instance (kube-state-metrics' own scrape
	// address) would repeat the exact meaningless-address bug this type
	// exists to fix, so a namespace-only selector is used instead: broader
	// than pod-scoped, but real.
	a := Alert{Labels: map[string]string{
		"alertname":  "KubeDeploymentReplicasMismatch",
		"namespace":  "gitea",
		"deployment": "gitea",
		"instance":   "198.51.100.100:8080",
	}}
	display, selector, ok := a.AffectedIdentity()
	if !ok {
		t.Fatal("AffectedIdentity() ok=false, want true")
	}
	if display != "gitea/gitea" {
		t.Errorf("display = %q, want %q", display, "gitea/gitea")
	}
	if selector != `{namespace="gitea"}` {
		t.Errorf("selector = %q, want namespace-only selector", selector)
	}
}

func TestAffectedIdentity_StatefulSetWorkloadUsesNamespaceSelector(t *testing.T) {
	a := Alert{Labels: map[string]string{
		"alertname":   "KubeStatefulSetReplicasMismatch",
		"namespace":   "bifrost",
		"statefulset": "bifrost",
		"instance":    "198.51.100.100:8080",
	}}
	display, selector, ok := a.AffectedIdentity()
	if !ok {
		t.Fatal("AffectedIdentity() ok=false, want true")
	}
	if display != "bifrost/bifrost" {
		t.Errorf("display = %q, want %q", display, "bifrost/bifrost")
	}
	if selector != `{namespace="bifrost"}` {
		t.Errorf("selector = %q, want namespace-only selector", selector)
	}
}

func TestAffectedIdentity_PodLabelStillPreferredOverWorkloadLabels(t *testing.T) {
	// If an alert somehow carries both pod and deployment/statefulset
	// labels, the more specific pod-scoped selector must win.
	a := Alert{Labels: map[string]string{
		"namespace":  "gitea",
		"pod":        "gitea-585b7c9565-r2lc7",
		"deployment": "gitea",
	}}
	display, selector, ok := a.AffectedIdentity()
	if !ok {
		t.Fatal("AffectedIdentity() ok=false, want true")
	}
	if display != "gitea/gitea-585b7c9565-r2lc7" || selector != `{namespace="gitea",pod="gitea-585b7c9565-r2lc7"}` {
		t.Errorf("display=%q selector=%q, want pod-scoped identity", display, selector)
	}
}

func TestAffectedIdentity_PartialNamespacePodFallsBackToHost(t *testing.T) {
	// namespace without pod (or vice versa) isn't enough to build a useful
	// selector — fall back rather than querying Loki with a namespace-only
	// selector that matches every pod in it.
	a := Alert{Labels: map[string]string{
		"alertname": "InstanceDown",
		"namespace": "gitea",
		"host":      "192.0.2.6:9100",
	}}
	display, selector, ok := a.AffectedIdentity()
	if !ok {
		t.Fatal("AffectedIdentity() ok=false, want true")
	}
	if display != "192.0.2.6:9100" || selector != `{host="192.0.2.6:9100"}` {
		t.Errorf("display=%q selector=%q, want host fallback", display, selector)
	}
}

func TestAffectedIdentity_NoUsableLabels(t *testing.T) {
	a := Alert{Labels: map[string]string{"alertname": "Mystery"}}
	_, _, ok := a.AffectedIdentity()
	if ok {
		t.Error("AffectedIdentity() ok=true, want false when no host/instance/namespace+pod labels exist")
	}
}

func TestLogIdentity_SafeTerm(t *testing.T) {
	for _, ok := range []string{"192.0.2.6", "web-01.example.com:9100", "checkout-7d9f-abcde", "[::1]:9100", "db_01@site"} {
		if got, err := (LogIdentity{Host: ok}).SafeTerm(); err != nil || got != ok {
			t.Errorf("SafeTerm(%q) = %q, %v; want it accepted unchanged", ok, got, err)
		}
	}
	for _, bad := range []string{`a"b`, "a'b", `a\b`, "a/b", "a|b", "a`b", "a(b", "a)b", "a b", "a\tb", "a\nb", "a\x00b"} {
		if _, err := (LogIdentity{Host: bad}).SafeTerm(); err == nil || !errors.Is(fmt.Errorf("wrapped: %w", err), ErrUnsafeSearchTerm) {
			t.Errorf("SafeTerm(%q) = %v, want a refusal matching ErrUnsafeSearchTerm", bad, err)
		}
	}
	// Pod wins over host, same precedence as Term().
	if got, _ := (LogIdentity{Namespace: "ns", Pod: "p-1", Host: "h"}).SafeTerm(); got != "p-1" {
		t.Errorf("SafeTerm picked %q, want the pod", got)
	}
}

func TestLogIdentity_ForLogQuery(t *testing.T) {
	cases := map[string]string{
		"https://kmp.tw/health":               "kmp.tw",
		"https://kmp.tw:8443/health?x=1&y=2":  "kmp.tw",
		"http://user:pw@probe.example:80/p#f": "probe.example",
		"tcp://203.0.113.5:22":                "203.0.113.5",
		"http://[2001:db8::1]:9115/probe":     "2001:db8::1",
		"http://[::1]/":                       "::1",
		// Not URLs with a host: unchanged.
		"192.0.2.6:9100": "192.0.2.6:9100",
		"node1:9100":     "node1:9100",
		"web01":          "web01",
		"kmp.tw/health":  "kmp.tw/health",
		"http://":        "http://",
		"mailto:ops@x":   "mailto:ops@x",
	}
	for in, want := range cases {
		if got := (LogIdentity{Host: in}).ForLogQuery().Host; got != want {
			t.Errorf("ForLogQuery(%q).Host = %q, want %q", in, got, want)
		}
	}
	pod := LogIdentity{Namespace: "ns", Pod: "p-1"}
	if got := pod.ForLogQuery(); got != pod {
		t.Errorf("ForLogQuery changed a pod identity: %+v", got)
	}
}

func TestLogIdentity_ForLogQuery_ThenSafeTerm(t *testing.T) {
	if term, err := (LogIdentity{Host: "https://kmp.tw/health"}).ForLogQuery().SafeTerm(); err != nil || term != "kmp.tw" {
		t.Errorf("SafeTerm after ForLogQuery = %q, %v; want kmp.tw accepted", term, err)
	}
	if term, err := (LogIdentity{Host: "http://[2001:db8::1]:9115/x"}).ForLogQuery().SafeTerm(); err != nil || term != "2001:db8::1" {
		t.Errorf("IPv6: SafeTerm after ForLogQuery = %q, %v", term, err)
	}
	// A hostname that is itself unsafe is still refused.
	for _, bad := range []string{"http://a'b.example/health", "http://a(b).example/"} {
		if _, err := (LogIdentity{Host: bad}).ForLogQuery().SafeTerm(); !errors.Is(err, ErrUnsafeSearchTerm) {
			t.Errorf("%q: err = %v, want still refused after normalization", bad, err)
		}
	}
	// Loki: the selector follows the normalized host; host:port unchanged.
	if got := (LogIdentity{Host: "https://kmp.tw/health"}).ForLogQuery().LokiSelector(); got != `{host="kmp.tw"}` {
		t.Errorf("LokiSelector = %s", got)
	}
	if got := (LogIdentity{Host: "192.0.2.6:9100"}).ForLogQuery().LokiSelector(); got != `{host="192.0.2.6:9100"}` {
		t.Errorf("LokiSelector = %s", got)
	}
}
