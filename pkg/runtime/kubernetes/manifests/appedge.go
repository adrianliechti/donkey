package manifests

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/adrianliechti/donkey/internal/config"
)

// AppGatewayConfig provisions one standard Gateway API entrypoint per app.
// Namespace is always donkey's system namespace. A managed platform may
// mirror the tenant-owned Gateway (and its TLS Secret) into RouteNamespace;
// that is why the HTTPRoute parent namespace is configurable independently.
type AppGatewayConfig struct {
	Namespace      string
	ClassName      string
	NameTemplate   string
	RouteNamespace string
	IssuerName     string
	IssuerKind     string
	IssuerGroup    string
}

func (c AppGatewayConfig) Enabled() bool { return c.ClassName != "" }

// AppEdgeName is globally unique inside an installation's system namespace.
// App workload names are plain in the namespace-per-space placement, so an
// edge name based on the app alone would make "shop" in two spaces overwrite
// the same Certificate, Gateway and HTTPRoute. Dots are unambiguous here:
// donkey's logical names may contain dashes, but never dots.
func AppEdgeName(space, app string) string { return "donkey-app-" + space + "." + app }

func AppCertificateSecretName(space, app string) string { return AppEdgeName(space, app) + "-tls" }

// AppGatewayObjects returns the optional external edge for an app. It is
// exported because both a deploy and a console network change render the same
// objects; neither path should be able to leave the certificate or Gateway
// behind.
func AppGatewayObjects(in Input) ([]runtime.Object, error) {
	if !in.AppGateway.Enabled() || in.Domain == "" {
		return nil, nil
	}

	name := AppEdgeName(in.Space, in.App.Name)
	gatewayName, err := config.AppGatewayName(in.AppGateway.NameTemplate, in.AppGateway.Namespace, in.Space, in.App.Name, in.Name())
	if err != nil {
		return nil, err
	}
	edgeLabels := Labels(in.Space, in.App.Name, RoleEdge)
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
		"name":  gatewayName,
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
			"secretName": AppCertificateSecretName(in.Space, in.App.Name),
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
			"name":      gatewayName,
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
								"name":  AppCertificateSecretName(in.Space, in.App.Name),
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
									LabelNamespaceName: in.AppGateway.Namespace,
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

	return []runtime.Object{certificate, gateway, route}, nil
}
