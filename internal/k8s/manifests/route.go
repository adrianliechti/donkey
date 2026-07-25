package manifests

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// httpRoute exposes the app on its hostname through the shared donkey
// Gateway (Gateway API — no Ingress).
func httpRoute(in Input) *gatewayv1.HTTPRoute {
	app := in.App
	gwNamespace := gatewayv1.Namespace(in.GatewayNamespace)
	port := gatewayv1.PortNumber(80)
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
					Name:      gatewayv1.ObjectName(in.GatewayName),
					Namespace: &gwNamespace,
				}},
			},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(in.Domain)},
			Rules: []gatewayv1.HTTPRouteRule{{
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
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      OIDCSecretName(in.App.Name),
			Namespace: in.Namespace,
			Labels:    Labels(in.Space, in.App.Name, "oidc"),
		},
		StringData: map[string]string{"client-secret": in.OIDC.ClientSecret},
	}
}

// securityPolicy makes Envoy itself run the OIDC login flow for the app's
// route. Built as unstructured to avoid importing the whole Envoy Gateway
// module for one CRD.
func securityPolicy(in Input) *unstructured.Unstructured {
	app := in.App
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.envoyproxy.io/v1alpha1",
		"kind":       "SecurityPolicy",
		"metadata": map[string]any{
			"name":      app.Name,
			"namespace": in.Namespace,
			"labels":    toAnyMap(Labels(in.Space, app.Name, ComponentWeb)),
		},
		"spec": map[string]any{
			"targetRefs": []any{map[string]any{
				"group": "gateway.networking.k8s.io",
				"kind":  "HTTPRoute",
				"name":  app.Name,
			}},
			"oidc": map[string]any{
				"provider": map[string]any{
					"issuer": in.OIDC.Issuer,
				},
				"clientID": in.OIDC.ClientID,
				"clientSecret": map[string]any{
					"name": OIDCSecretName(app.Name),
				},
				"redirectURL": "https://" + in.Domain + "/oauth2/callback",
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
