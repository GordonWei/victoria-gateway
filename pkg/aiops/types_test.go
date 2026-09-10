package aiops

import "testing"

func TestAffectedIdentity_NamespacePodPreferredOverInstance(t *testing.T) {
	// A kube-state-metrics-sourced alert (e.g. KubePodCrashLooping) carries
	// both namespace/pod AND an instance label — but that instance is
	// kube-state-metrics' own scrape address, not the affected pod's host.
	// namespace+pod must win.
	a := Alert{Labels: map[string]string{
		"alertname": "KubePodCrashLooping",
		"namespace": "gitea",
		"pod":       "gitea-585b7c9565-r2lc7",
		"instance":  "172.16.18.100:8080",
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
		"instance":  "172.16.100.6:9222",
	}}
	display, selector, ok := a.AffectedIdentity()
	if !ok {
		t.Fatal("AffectedIdentity() ok=false, want true")
	}
	if display != "172.16.100.6:9222" {
		t.Errorf("display = %q, want %q", display, "172.16.100.6:9222")
	}
	if selector != `{host="172.16.100.6:9222"}` {
		t.Errorf("selector = %q", selector)
	}
}

func TestAffectedIdentity_PartialNamespacePodFallsBackToHost(t *testing.T) {
	// namespace without pod (or vice versa) isn't enough to build a useful
	// selector — fall back rather than querying Loki with a namespace-only
	// selector that matches every pod in it.
	a := Alert{Labels: map[string]string{
		"alertname": "InstanceDown",
		"namespace": "gitea",
		"host":      "172.16.100.6:9100",
	}}
	display, selector, ok := a.AffectedIdentity()
	if !ok {
		t.Fatal("AffectedIdentity() ok=false, want true")
	}
	if display != "172.16.100.6:9100" || selector != `{host="172.16.100.6:9100"}` {
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
