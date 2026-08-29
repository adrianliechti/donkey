package manifests

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		t.Error("donkey-ai needs the credential it swaps each space's token for")
	}
}

// Apps reach donkey-ai; only the gateway reaches the admin API. Before the
// split the API had to admit apps, because the AI proxy shared its listener.
func TestAdminAPIIsGatewayOnly(t *testing.T) {
	in := systemInput()
	in.WorkloadNamespace = "donkey-apps"

	server := serverNetworkPolicy(in)
	if n := len(server.Spec.Ingress[0].From); n != 1 {
		t.Fatalf("the admin API should admit exactly one peer, got %d", n)
	}
	if peer := server.Spec.Ingress[0].From[0]; peer.PodSelector == nil ||
		peer.PodSelector.MatchLabels[LabelComponent] != GatewayAppName {
		t.Errorf("the admin API's only peer should be the gateway, got %+v", peer)
	}

	ai := aiNetworkPolicy(in)
	peer := ai.Spec.Ingress[0].From[0]
	if peer.NamespaceSelector == nil ||
		peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "donkey-apps" {
		t.Errorf("donkey-ai should admit the namespace apps live in, got %+v", peer)
	}
	if len(peer.PodSelector.MatchExpressions) != 1 ||
		peer.PodSelector.MatchExpressions[0].Key != LabelSpace ||
		peer.PodSelector.MatchExpressions[0].Operator != metav1.LabelSelectorOpExists {
		t.Errorf("the tenant peer must require a logical space, got %+v", peer.PodSelector)
	}
}

// The console's chat is browser code: it calls its own origin, and
// donkey-server forwards that to donkey-ai. The server must therefore be
// told where donkey-ai is, and must be allowed to reach it — while the
// endpoint apps use stays plain /v1 on donkey-ai itself.
func TestServerForwardsToTheAIEndpoint(t *testing.T) {
	in := systemInput()
	rendered := componentNames(in)
	if !strings.Contains(rendered, "DONKEY_AI_URL") ||
		!strings.Contains(rendered, "http://donkey-ai."+DefaultSystemNamespace+".svc:80") {
		t.Error("donkey-server needs donkey-ai's address to forward the console's /ai/v1")
	}
	// No gateway route for it any more: the server owns every path on its
	// own hostname.
	for _, obj := range SystemObjects(in) {
		if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Annotations[AnnotationPath] == "/ai/v1" {
			t.Error("the console's origin is the server's — /ai/v1 should not be a gateway route")
		}
	}

	// And the server may actually reach it.
	var reachesAI bool
	for _, peer := range aiNetworkPolicy(in).Spec.Ingress[0].From {
		if peer.PodSelector != nil && peer.PodSelector.MatchLabels[LabelComponent] == ServerName {
			reachesAI = true
		}
	}
	if !reachesAI {
		t.Error("donkey-server must be admitted to donkey-ai, or the console's chat cannot be forwarded")
	}
}
