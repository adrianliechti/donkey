package manifests

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// AppGatewayConfig provisions one standard Gateway API entrypoint per app.
// Namespace is always donkey's system namespace. A managed platform may
// mirror the tenant-owned Gateway (and its TLS Secret) into RouteNamespace;
// that is why the HTTPRoute parent namespace is configurable independently.
type AppGatewayConfig struct {
	Namespace      string
	ClassName      string
	RouteNamespace string
	IssuerName     string
	IssuerKind     string
	IssuerGroup    string
}

func (c AppGatewayConfig) Enabled() bool { return c.ClassName != "" }

func AppEdgeName(app string) string { return "donkey-app-" + app }

func AppGatewayName(namespace, app string) string {
	return fmt.Sprintf("%s-%s-gw", namespace, app)
}

func AppCertificateSecretName(app string) string { return AppEdgeName(app) + "-tls" }

// AppGatewayObjects returns the optional external edge for an app. It is
// exported because both a deploy and a console network change render the same
// objects; neither path should be able to leave the certificate or Gateway
// behind.
func AppGatewayObjects(in Input) []runtime.Object {
	if !in.AppGateway.Enabled() || in.Domain == "" {
		return nil
	}

	name := AppEdgeName(in.Name())
	edgeLabels := Labels(in.Space, in.App.Name, ComponentEdge)
	// cert-manager copies secretTemplate labels to the generated TLS Secret.
	// The apply/render boundary adds the installation label to both maps.
	labels := toAnyMap(edgeLabels)
	issuerKind := in.AppGateway.IssuerKind
	if issuerKind == "" {
		issuerKind = "ClusterIssuer"
	}
	issuerGroup := in.AppGateway.IssuerGroup
	if issuerGroup == "" {
		issuerGroup = "cert-manager.io"
	}
	parent := map[string]any{
		"group": "gateway.networking.k8s.io",
		"kind":  "Gateway",
		"name":  AppGatewayName(in.AppGateway.Namespace, in.Name()),
	}
	if in.AppGateway.RouteNamespace != "" {
		parent["namespace"] = in.AppGateway.RouteNamespace
	}

	certificate := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]any{
			"name":      name,
			"namespace": in.AppGateway.Namespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"secretName": AppCertificateSecretName(in.Name()),
			"secretTemplate": map[string]any{
				"labels": labels,
			},
			// Key type, lifetime, renewal window and usages are deliberately
			// left to cert-manager and the configured issuer. Enterprise PKI
			// policies commonly constrain them more tightly than public CAs.
			"dnsNames": []any{in.Domain},
			"issuerRef": map[string]any{
				"name":  in.AppGateway.IssuerName,
				"kind":  issuerKind,
				"group": issuerGroup,
			},
		},
	}}

	gateway := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "Gateway",
		"metadata": map[string]any{
			"name":      AppGatewayName(in.AppGateway.Namespace, in.Name()),
			"namespace": in.AppGateway.Namespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"gatewayClassName": in.AppGateway.ClassName,
			"listeners": []any{
				map[string]any{
					"name":     "https",
					"hostname": in.Domain,
					"protocol": "HTTPS",
					"port":     int64(443),
					"tls": map[string]any{
						"mode": "Terminate",
						"certificateRefs": []any{
							map[string]any{
								"group": "",
								"kind":  "Secret",
								"name":  AppCertificateSecretName(in.Name()),
							},
						},
					},
					// Same-namespace is Gateway API's safe default. The explicit
					// selector preserves that boundary if a managed platform mirrors
					// this Gateway into RouteNamespace without rewriting its listener.
					"allowedRoutes": map[string]any{
						"namespaces": map[string]any{
							"from": "Selector",
							"selector": map[string]any{
								"matchLabels": map[string]any{
									"kubernetes.io/metadata.name": in.AppGateway.Namespace,
								},
							},
						},
					},
				},
			},
		},
	}}

	route := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"metadata": map[string]any{
			"name":      name,
			"namespace": in.AppGateway.Namespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"parentRefs": []any{parent},
			"hostnames":  []any{in.Domain},
			"rules": []any{
				map[string]any{
					"timeouts": map[string]any{
						"request":        "600s",
						"backendRequest": "600s",
					},
					"backendRefs": []any{
						map[string]any{
							"kind": "Service",
							"name": GatewayAppName,
							"port": int64(80),
						},
					},
				},
			},
		},
	}}

	return []runtime.Object{certificate, gateway, route}
}
