package manifests

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A cluster that already runs a Gateway API edge (and hands out namespaces
// rather than LoadBalancer services) usually wants traffic to enter through
// that edge, not through a Service donkey exposes itself. GatewayRef names
// that gateway; donkey then renders one HTTPRoute attaching the donkey
// gateway to it, and the donkey gateway goes on doing what it always does —
// routing, login, access — one hop further in.
//
// The objects are built as unstructured: HTTPRoute is a CRD, and donkey
// neither needs the typed client nor wants the dependency for one object.

// GatewayRef names the Gateway API gateway the installation attaches to.
type GatewayRef struct {
	Name string
	// Namespace is where that gateway lives; empty means the gateway is
	// expected in donkey's own system namespace (the Gateway API default of
	// "same namespace as the route").
	Namespace string
	// SectionName narrows the attachment to one listener of the gateway.
	SectionName string
}

// Attached reports whether the installation routes through a cluster gateway.
func (g GatewayRef) Attached() bool { return g.Name != "" }

// httpRoute sends the installation's hostnames to the donkey gateway: the
// domain itself and everything under it (a Gateway API wildcard matches one
// or more labels, so <app>.<space>.<domain> is covered at any depth).
//
// The backend is the gateway's plain-http port: an HTTPRoute is matched after
// the cluster gateway terminates TLS, so this pairs with tls.mode "none" —
// custom app hostnames outside the domain are not covered and keep needing
// donkey's own edge.
func httpRoute(in SystemInput) *unstructured.Unstructured {
	parent := map[string]any{"name": in.HTTPRouteGateway.Name}
	if in.HTTPRouteGateway.Namespace != "" {
		parent["namespace"] = in.HTTPRouteGateway.Namespace
	}
	if in.HTTPRouteGateway.SectionName != "" {
		parent["sectionName"] = in.HTTPRouteGateway.SectionName
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"metadata": map[string]any{
			"name":      GatewayAppName,
			"namespace": in.Namespace,
			"labels":    toAnyMap(SystemLabels(GatewayAppName)),
		},
		"spec": map[string]any{
			"parentRefs": []any{parent},
			"hostnames":  []any{in.Domain, "*." + in.Domain},
			"rules": []any{
				map[string]any{
					"backendRefs": []any{
						map[string]any{"name": GatewayAppName, "port": int64(80)},
					},
				},
			},
		},
	}}
}

// toAnyMap is what unstructured wants labels as.
func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
