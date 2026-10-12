package manifests

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// The fence admits exactly three peers: the space itself, the gateway and
// donkey-server. Everything else — other spaces first of all — is out.
func TestSpaceIsolationPolicyAdmitsSpaceGatewayAndServer(t *testing.T) {
	p := SpaceIsolationPolicy("acme", "donkey-acme", "donkey-system", false)

	if p.Name != "acme-space" {
		t.Errorf("name = %q, want acme-space", p.Name)
	}
	if got := p.Spec.PodSelector.MatchLabels; got[LabelSpace] != "acme" || len(got) != 1 {
		t.Errorf("pod selector = %v, want the space alone — the apply boundary stamps the installation", got)
	}
	if len(p.Spec.PolicyTypes) != 1 || p.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("policy types = %v, want ingress only when egress is not restricted", p.Spec.PolicyTypes)
	}
	if len(p.Spec.Egress) != 0 {
		t.Errorf("egress rules = %v, want none when egress is not restricted", p.Spec.Egress)
	}
	if len(p.Spec.Ingress) != 1 {
		t.Fatalf("got %d ingress rules, want 1", len(p.Spec.Ingress))
	}
	if ports := p.Spec.Ingress[0].Ports; len(ports) != 0 {
		t.Errorf("ports = %v, want none — admitted peers reach every port", ports)
	}

	from := p.Spec.Ingress[0].From
	if len(from) != 3 {
		t.Fatalf("got %d peers, want same-space + gateway + server", len(from))
	}
	sameSpace := from[0]
	if sameSpace.NamespaceSelector != nil {
		t.Error("the same-space peer must name no namespace — it is read in the policy's own")
	}
	want := map[string]string{LabelInstallation: "donkey-system", LabelSpace: "acme"}
	if got := sameSpace.PodSelector.MatchLabels; len(got) != len(want) || got[LabelInstallation] != want[LabelInstallation] || got[LabelSpace] != want[LabelSpace] {
		t.Errorf("same-space peer = %v, want %v", got, want)
	}
	for i, name := range []string{GatewayAppName, ServerName} {
		peer := from[i+1]
		if got, want := peer.PodSelector.MatchLabels[LabelRole], SystemRole(name); got != want {
			t.Errorf("peer %d selects role %q, want %q", i+1, got, want)
		}
		if got := peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; got != "donkey-system" {
			t.Errorf("peer %d is pinned to namespace %q, want the control plane's", i+1, got)
		}
	}
}

// With egress restricted, the fence also confines outbound traffic: DNS, the
// space itself, and the two platform endpoints an app is meant to reach —
// never the internet or another space.
func TestSpaceIsolationPolicyRestrictsEgressToDNSSpaceAndPlatform(t *testing.T) {
	p := SpaceIsolationPolicy("acme", "donkey-acme", "donkey-system", true)

	types := map[networkingv1.PolicyType]bool{}
	for _, t := range p.Spec.PolicyTypes {
		types[t] = true
	}
	if !types[networkingv1.PolicyTypeIngress] || !types[networkingv1.PolicyTypeEgress] {
		t.Fatalf("policy types = %v, want both ingress and egress", p.Spec.PolicyTypes)
	}
	// Ingress is unchanged by the egress toggle.
	if len(p.Spec.Ingress) != 1 || len(p.Spec.Ingress[0].From) != 3 {
		t.Errorf("egress restriction must not change the ingress rule: %+v", p.Spec.Ingress)
	}
	if len(p.Spec.Egress) != 5 {
		t.Fatalf("got %d egress rules, want DNS + same-space + AI + telemetry + service proxy", len(p.Spec.Egress))
	}

	// DNS: any destination, both protocols on 53, and no peer restriction.
	dns := p.Spec.Egress[0]
	if len(dns.To) != 0 {
		t.Errorf("DNS rule pins a destination (%v); it must reach any resolver", dns.To)
	}
	protos := map[corev1.Protocol]bool{}
	for _, port := range dns.Ports {
		protos[*port.Protocol] = true
		if port.Port.IntValue() != dnsPort {
			t.Errorf("DNS port = %v, want %d", port.Port, dnsPort)
		}
	}
	if !protos[corev1.ProtocolUDP] || !protos[corev1.ProtocolTCP] {
		t.Errorf("DNS rule = %v, want UDP and TCP", dns.Ports)
	}

	// Same-space egress reuses the installation+space peer (covers postgres,
	// S3 storage and sibling apps — all same-space pods).
	same := p.Spec.Egress[1].To
	if len(same) != 1 || same[0].NamespaceSelector != nil ||
		same[0].PodSelector.MatchLabels[LabelSpace] != "acme" ||
		same[0].PodSelector.MatchLabels[LabelInstallation] != "donkey-system" {
		t.Errorf("same-space egress peer = %+v", same)
	}

	// The platform endpoints are separate so each gets only its actual
	// workload port (NetworkPolicy sees the target Pod port after Service
	// translation).
	for i, name := range []string{AIAppName, TelemetryAppName, ProxyAppName} {
		rule := p.Spec.Egress[i+2]
		if len(rule.To) != 1 || len(rule.Ports) != 1 {
			t.Fatalf("platform rule %d = %+v, want one peer and one port", i, rule)
		}
		if got, want := rule.To[0].PodSelector.MatchLabels[LabelRole], SystemRole(name); got != want {
			t.Errorf("platform egress peer %d selects role %q, want %q", i, got, want)
		}
		if got := rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; got != "donkey-system" {
			t.Errorf("platform egress peer %d namespace = %q, want the control plane's", i, got)
		}
		wantPort := int(aiPort)
		if name == TelemetryAppName {
			wantPort = int(telemetryOTLPPort)
		}
		if name == ProxyAppName {
			wantPort = proxyPort
		}
		if got := rule.Ports[0].Port.IntValue(); got != wantPort {
			t.Errorf("platform egress peer %d port = %d, want %d", i, got, wantPort)
		}
	}
}

// The fence rides with the app's objects, so `--dry-run` renders exactly what
// deploy applies — and only when the installation asks for it.
func TestAppObjectsRenderTheSpaceFenceOnlyWhenAsked(t *testing.T) {
	find := func(objs []runtime.Object) *networkingv1.NetworkPolicy {
		for _, o := range objs {
			if p, ok := o.(*networkingv1.NetworkPolicy); ok {
				return p
			}
		}
		return nil
	}

	in := testInput(t)
	objs, err := AppObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	if find(objs) != nil {
		t.Error("an installation that doesn't isolate spaces must render no fence")
	}

	in.IsolateSpace = true
	in.SystemNamespace = "donkey-system"
	objs, err = AppObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	p := find(objs)
	if p == nil {
		t.Fatal("isolation asked for, but no NetworkPolicy rendered")
	}
	if p.Namespace != in.Namespace || p.Name != SpaceIsolationName(in.Space) {
		t.Errorf("fence is %s/%s, want %s/%s", p.Namespace, p.Name, in.Namespace, SpaceIsolationName(in.Space))
	}

	// The stamped pod selector is what keeps the fence off pods donkey
	// doesn't own in a namespace it was given.
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(p)
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: m}
	if err := StampInstallation(u, "donkey-system"); err != nil {
		t.Fatal(err)
	}
	sel, _, err := unstructured.NestedStringMap(u.Object, "spec", "podSelector", "matchLabels")
	if err != nil {
		t.Fatal(err)
	}
	if sel[LabelInstallation] != "donkey-system" || sel[LabelSpace] != in.Space {
		t.Errorf("stamped pod selector = %v, want space plus installation", sel)
	}
}

// A space's fence carries the space's own labels, so the sweep that removes a
// space from a shared namespace removes the fence with it.
func TestSpaceIsolationPolicyIsASpaceObject(t *testing.T) {
	p := SpaceIsolationPolicy("acme", "tenant-apps", "donkey-system", false)
	want := SpaceLabels("acme")
	if len(p.Labels) != len(want) {
		t.Fatalf("labels = %v, want the space's own (%v)", p.Labels, want)
	}
	for k, v := range want {
		if p.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, p.Labels[k], v)
		}
	}
	if _, ok := p.Labels[LabelApp]; ok {
		t.Error("the fence belongs to the space, never to one app")
	}
	var _ metav1.Object = p // it is swept by name and label like any object
}
