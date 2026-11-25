package manifests

import (
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

// The point of the split: the endpoint tenant code reaches is not the
// process that can write to the cluster.
func TestAIEndpointIsSeparateAndNarrow(t *testing.T) {
	in := systemInput()
	in.WorkloadNamespace = "donkey-apps"
	in.SkipNamespace = true

	var aiRules []rbacv1.PolicyRule
	for _, obj := range SystemObjects(in) {
		if role, ok := obj.(*rbacv1.Role); ok && role.Name == AIAppName {
			aiRules = role.Rules
		}
	}
	if len(aiRules) == 0 {
		t.Fatal("donkey-ai should get a Role of its own")
	}
	for _, rule := range aiRules {
		for _, verb := range rule.Verbs {
			switch verb {
			case "get", "list", "watch":
			default:
				t.Errorf("donkey-ai must never hold a write verb, found %q", verb)
			}
		}
		for _, res := range rule.Resources {
			if res != "secrets" {
				t.Errorf("donkey-ai only needs the binding Secrets, found %q", res)
			}
		}
	}

	rendered := componentNames(in)
	if !strings.Contains(rendered, `- ai`) {
		t.Error("donkey-ai should run the ai entrypoint of the same image")
	}
	// It holds the upstream credential, and nothing the admin API holds.
	if !strings.Contains(rendered, "OPENAI_API_KEY") {
		t.Error("donkey-ai needs the credential it uses for development machines")
	}
}

// Without development machines only the gateway reaches the admin API.
func TestAdminAPIIsGatewayOnly(t *testing.T) {
	in := systemInput()
	in.WorkloadNamespace = "donkey-apps"

	server := serverNetworkPolicy(in)
	if n := len(server.Spec.Ingress[0].From); n != 1 {
		t.Fatalf("the admin API should admit exactly one peer, got %d", n)
	}
	if peer := server.Spec.Ingress[0].From[0]; peer.PodSelector == nil ||
		peer.PodSelector.MatchLabels[LabelRole] != SystemRole(GatewayAppName) {
		t.Errorf("the admin API's only peer should be the gateway, got %+v", peer)
	}

	if len(aiNetworkPolicy(in).Spec.Ingress) != 0 {
		t.Fatal("the model proxy must deny ingress when machines are disabled")
	}
}
