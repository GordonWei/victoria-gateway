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
		"instance":   "172.16.18.100:8080",
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
		"instance":    "172.16.18.100:8080",
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
