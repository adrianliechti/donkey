package manifests

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// httpRoute exposes the app on its hostname through the shared donkey
// Gateway (Gateway API — no Ingress). It attaches to the https listener only:
// the login gateway's cookies are Secure-only, so serving the app over plain
// http was never right — the shared redirect route sends port-80 visitors to
// https instead. Streaming responses (SSE, long downloads) must not be cut by
// Envoy's default request timeout, so it is disabled like on the API's route.
func httpRoute(in Input) *gatewayv1.HTTPRoute {
	app := in.App
	gwNamespace := gatewayv1.Namespace(in.GatewayNamespace)
	section := gatewayv1.SectionName(HTTPSListenerName)
	port := gatewayv1.PortNumber(80)
	noTimeout := gatewayv1.Duration("0s")
	return &gatewayv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: in.Namespace,
			Labels:    Labels(in.Space, app.Name, ComponentWeb),
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{
					Name:        gatewayv1.ObjectName(in.GatewayName),
					Namespace:   &gwNamespace,
					SectionName: &section,
				}},
			},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(in.Domain)},
			Rules: []gatewayv1.HTTPRouteRule{{
				Timeouts: &gatewayv1.HTTPRouteTimeouts{Request: &noTimeout},
				BackendRefs: []gatewayv1.HTTPBackendRef{{
					BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: gatewayv1.ObjectName(app.Name),
							Port: &port,
						},
					},
				}},
			}},
		},
	}
}

func OIDCSecretName(app string) string { return app + "-oidc" }

// oidcSecret carries the OIDC client secret into the app namespace — Envoy's
// SecurityPolicy can only reference secrets next to it.
func oidcSecret(in Input) *corev1.Secret {
	// A dry run renders without credentials, so say so rather than showing an
	// empty value somebody might copy into a real manifest.
	secret := in.OIDC.ClientSecret
	if secret == "" {
		secret = "(withheld — a dry run never prints credentials)"
	}
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      OIDCSecretName(in.App.Name),
			Namespace: in.Namespace,
			Labels:    Labels(in.Space, in.App.Name, "oidc"),
		},
		StringData: map[string]string{"client-secret": secret},
	}
}

// securityPolicy makes Envoy itself run the OIDC login flow for the app's
// route. Built as unstructured to avoid importing the whole Envoy Gateway
// module for one CRD.
func securityPolicy(in Input) *unstructured.Unstructured {
	return securityPolicyFor(in, in.App.Name, in.Domain, ComponentWeb)
}

// securityPolicyFor protects one named HTTPRoute. Policies attach per route,
// which is what lets an app's own address be private while a path it shares
// with another app stays open (or the other way round). The callback URL has
// to be on the host the browser is actually visiting.
func securityPolicyFor(in Input, routeName, host, component string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.envoyproxy.io/v1alpha1",
		"kind":       "SecurityPolicy",
		"metadata": map[string]any{
			"name":      routeName,
			"namespace": in.Namespace,
			"labels":    toAnyMap(Labels(in.Space, in.App.Name, component)),
		},
		"spec": map[string]any{
			"targetRefs": []any{map[string]any{
				"group": "gateway.networking.k8s.io",
				"kind":  "HTTPRoute",
				"name":  routeName,
			}},
			"oidc": map[string]any{
				"provider": map[string]any{
					"issuer": in.OIDC.Issuer,
				},
				"clientID": in.OIDC.ClientID,
				"clientSecret": map[string]any{
					"name": OIDCSecretName(in.App.Name),
				},
				"redirectURL": "https://" + host + "/oauth2/callback",
			},
		},
	}}
}

func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
