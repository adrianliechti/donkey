package manifests

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Space isolation (kubernetes.isolateSpaces) walls spaces off from each other
// on the network: a space's pods accept traffic only from their own space,
// the donkey gateway and donkey-server. Cross-space calls keep working the
// sanctioned way — through the gateway on an app's public hostname, where
// login and access rules apply — they just stop working as anonymous
// pod-to-pod traffic.
//
// Ingress is the always-on half. Kubelet probes originate on the node, not in
// a pod, and CNIs exempt them — the fence never breaks readiness. Enforcement
// is the CNI's business: a network plugin that ignores NetworkPolicy applies
// this as a no-op.
//
// Egress (kubernetes.restrictEgress) is the opt-in half, off by default
// because it is the disruptive one: it confines a space's pods to their own
// space, the platform services an app legitimately uses (donkey-ai for the
// openai addon, donkey-telemetry for metrics) and DNS — and cuts everything
// else, the public internet included. An app that calls a third-party API at
// runtime needs it left off. When it is off the policy carries no egress
// section at all, so egress stays wide open.

// dnsPort is where cluster DNS answers. Egress to it is allowed to any
// destination, not a named resolver: clusters run CoreDNS, a node-local DNS
// cache at a link-local address, or both, and pinning one breaks the others.
const dnsPort = 53

// SpaceIsolationName names the space's NetworkPolicy — the space marker's
// dotted spelling, which no app or space name can collide with.
func SpaceIsolationName(space string) string { return spaceMarkerPrefix + space }

// SpaceIsolationPolicy fences one space. installation is the control plane's
// namespace — the installation identity every donkey object is stamped with.
// restrictEgress adds the egress half; without it the policy governs ingress
// only and egress is unrestricted.
//
// The policy's own pod selector names only the space; the apply/render
// boundary stamps the installation into it (see StampInstallation), so in a
// namespace donkey was given it never constrains pods it doesn't own. The
// peers spell the installation out themselves — stamping doesn't reach into
// ingress or egress rules.
func SpaceIsolationPolicy(space, namespace, installation string, restrictEgress bool) *networkPolicy {
	// The same-space peer names no namespace: a peer without one is read in
	// the policy's own namespace, which is where all of the space's pods
	// live in either placement arrangement.
	sameSpace := networkingv1.NetworkPolicyPeer{
		PodSelector: &metav1.LabelSelector{MatchLabels: SpacePodSelector(installation, space)},
	}
	policy := &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      SpaceIsolationName(space),
			Namespace: namespace,
			Labels:    SpaceLabels(space),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{LabelSpace: space}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{
					sameSpace,
					// The gateway carries every route's traffic; the server
					// dials apps and services directly for tunnels and the
					// data consoles.
					systemPeer(installation, GatewayAppName),
					systemPeer(installation, ServerName),
				},
			}},
		},
	}
	if !restrictEgress {
		return policy
	}
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	dns := intstr.FromInt32(dnsPort)
	ai := intstr.FromInt32(aiPort)
	otlp := intstr.FromInt32(telemetryOTLPPort)
	policy.Spec.PolicyTypes = append(policy.Spec.PolicyTypes, networkingv1.PolicyTypeEgress)
	policy.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{
		{
			// DNS first, or nothing an app does resolves. Any destination on
			// port 53 — see dnsPort — over both protocols.
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &udp, Port: &dns},
				{Protocol: &tcp, Port: &dns},
			},
		},
		{
			// Its own space: sibling apps and the space's services
			// (postgres, rustfs) are all same-space pods.
			To: []networkingv1.NetworkPolicyPeer{sameSpace},
		},
		{
			// The AI proxy the openai addon points at. The policy sees the
			// target Pod port after Service translation.
			To:    []networkingv1.NetworkPolicyPeer{systemPeer(installation, AIAppName)},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &ai}},
		},
		{
			// Apps may submit OTLP, but the telemetry query API remains a
			// server-only control-plane endpoint.
			To:    []networkingv1.NetworkPolicyPeer{systemPeer(installation, TelemetryAppName)},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &otlp}},
		},
	}
	return policy
}
