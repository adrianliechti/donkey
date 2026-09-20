package manifests

import (
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/adrianliechti/donkey/internal/addons"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

const RoleProvisioning = "provisioning"

// AddonEgressPolicy adds only the provisioned destinations to a space's
// existing egress fence. Callers must not install it when egress is open:
// selecting a pod with ANY egress policy would otherwise start isolating it.
func AddonEgressPolicy(space, namespace, installation, name, objectName string, info *addonv1.ProvisionResponse) (*networkingv1.NetworkPolicy, error) {
	if err := addons.ValidateProvisioning(info); err != nil {
		return nil, err
	}
	policy := &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name: objectName + "-egress", Namespace: namespace,
			Labels: AddonLabels(space, name, RoleProvisioning),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: SpacePodSelector(installation, space)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		},
	}
	for _, endpoint := range info.Endpoints {
		port := endpoint.Port
		var peers []networkingv1.NetworkPolicyPeer
		if target := endpoint.Kubernetes; target != nil {
			if target.Port != 0 {
				port = target.Port
			}
			// Namespace and pod selectors MUST be in the same peer: two
			// peers would mean OR, admitting unrelated pods/namespaces.
			peers = append(peers, networkingv1.NetworkPolicyPeer{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{LabelNamespaceName: target.Namespace}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: maps.Clone(target.PodLabels)},
			})
		} else {
			for _, cidr := range endpoint.Cidrs {
				peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
			}
		}
		if len(peers) == 0 {
			return nil, fmt.Errorf("addon %s needs Kubernetes pod selectors or explicit CIDRs for every endpoint while egress is restricted", name)
		}
		protocol := corev1.Protocol(endpoint.Protocol)
		if protocol == "" {
			protocol = corev1.ProtocolTCP
		}
		p := intstr.FromInt32(port)
		policy.Spec.Egress = append(policy.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
			To: peers, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocol, Port: &p}},
		})
	}
	return policy, nil
}
