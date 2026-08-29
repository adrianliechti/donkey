package manifests

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Who may reach donkey's own workloads. A NetworkPolicy only default-denies
// the pods it selects, so each of these constrains one deployment's ingress
// and nothing else in the namespace — donkey never changes how a cluster
// treats traffic it doesn't own.

// networkPolicy is the type these render; named so the callers read as
// intent rather than as apimachinery.
type networkPolicy = networkingv1.NetworkPolicy

// ingressPolicy admits the named peers to one port of one donkey workload,
// and nothing else to it.
func ingressPolicy(in SystemInput, name string, port int32, peers []networkingv1.NetworkPolicyPeer) *networkPolicy {
	p := intstr.FromInt32(port)
	tcp := corev1.ProtocolTCP
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: in.Namespace,
			Labels:    SystemLabels(name),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: SystemSelector(name)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  peers,
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &p}},
			}},
		},
	}
}

// systemPeer is one of donkey's own workloads, in the control plane's
// namespace (which is also the installation identity — pinning the namespace
// is what keeps a second installation's same-named pods out).
func systemPeer(namespace, name string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
			LabelInstallation: namespace,
			LabelComponent:    name,
		}},
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace}},
	}
}

// appPeers is where this installation's apps run. With one namespace for
// every app that is a single name; with a namespace per space it is the
// installation label donkey stamps on each namespace. The pod selector is
// retained in both modes: a manually-created workload in one of those
// namespaces is not silently granted access to control-plane endpoints.
func appPeers(in SystemInput) []networkingv1.NetworkPolicyPeer {
	pods := &metav1.LabelSelector{
		MatchLabels: map[string]string{LabelInstallation: in.Namespace},
		// The system namespace carries the installation label too. Requiring a
		// logical space keeps other control-plane pods out of the broad tenant
		// peer and leaves only the explicitly named system peers below.
		MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key: LabelSpace, Operator: metav1.LabelSelectorOpExists,
		}},
	}
	if in.Shared() {
		return []networkingv1.NetworkPolicyPeer{{
			PodSelector: pods,
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": in.WorkloadNamespace},
			},
		}}
	}
	return []networkingv1.NetworkPolicyPeer{{
		PodSelector: pods,
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{LabelInstallation: in.Namespace},
		},
	}}
}

// serverNetworkPolicy is the admin API's second lock. The first is the
// credential: every /v1 route verifies a sealed token. This says who may even
// try — the gateway, through which every browser and CLI request arrives.
//
// Apps are deliberately absent. They used to belong here because the AI proxy
// shared this listener, which is what put the admin API in reach of tenant
// code; it now runs as donkey-ai, and nothing an app needs lives here.
func serverNetworkPolicy(in SystemInput) *networkPolicy {
	return ingressPolicy(in, ServerName, serverPort, []networkingv1.NetworkPolicyPeer{
		systemPeer(in.Namespace, GatewayAppName),
	})
}

// buildkitNetworkPolicy is the one policy donkey cannot do without: the build
// engine speaks unauthenticated gRPC and runs privileged, so anything that
// can reach its port can run code on a node. Only donkey-server may.
func buildkitNetworkPolicy(in SystemInput) *networkPolicy {
	return ingressPolicy(in, buildkitName, buildkitPort, []networkingv1.NetworkPolicyPeer{
		systemPeer(in.Namespace, ServerName),
	})
}
