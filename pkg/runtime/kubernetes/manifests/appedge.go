package manifests

import (
	"fmt"

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

// AppEdgeObjects renders the configured per-host edge. The two strategies are
// intentionally exclusive: otherwise two independent proxies would claim the
// same hostname and DNS would decide which configuration happened to answer.
func AppEdgeObjects(in Input) ([]runtime.Object, error) {
	if in.AppGateway.Enabled() && in.ContourHTTPProxy.Enabled() {
		return nil, fmt.Errorf("appGateway and contourHTTPProxy cannot both expose %s", in.Domain)
	}
	if in.ContourHTTPProxy.Enabled() {
		return ContourHTTPProxyObjects(in), nil
	}
	return AppGatewayObjects(in)
}

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
	parent := map[string]any{
		"group": "gateway.networking.k8s.io",
		"kind":  "Gateway",
		"name":  gatewayName,
	}
	if in.AppGateway.RouteNamespace != "" {
		parent["namespace"] = in.AppGateway.RouteNamespace
	}

	certificate := managedCertificate(name, in.AppGateway.Namespace,
		AppCertificateSecretName(in.Space, in.App.Name), []string{in.Domain}, edgeLabels,
		in.AppGateway.IssuerName, in.AppGateway.IssuerKind, in.AppGateway.IssuerGroup)

	gateway := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "Gateway",
		"metadata": map[string]any{
			"name":      gatewayName,
			"namespace": in.AppGateway.Namespace,
			"labels":    toAnyMap(edgeLabels),
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
			"labels":    toAnyMap(edgeLabels),
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

// managedCertificate is shared by both external-edge strategies and the
// Contour control-plane edge. cert-manager copies secretTemplate labels to
// the generated TLS Secret; the apply/render boundary adds the installation
// label to both maps.
func managedCertificate(name, namespace, secretName string, dnsNames []string, objectLabels map[string]string,
	issuerName, issuerKind, issuerGroup string,
) *unstructured.Unstructured {
	if issuerKind == "" {
		issuerKind = "ClusterIssuer"
	}
	if issuerGroup == "" {
		issuerGroup = "cert-manager.io"
	}
	names := make([]any, 0, len(dnsNames))
	for _, host := range dnsNames {
		if host != "" {
			names = append(names, host)
		}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    toAnyMap(objectLabels),
		},
		"spec": map[string]any{
			"secretName": secretName,
			"secretTemplate": map[string]any{
				"labels": toAnyMap(objectLabels),
			},
			// Key type, lifetime, renewal window and usages are deliberately
			// left to cert-manager and the configured issuer. Enterprise PKI
			// policies commonly constrain them more tightly than public CAs.
			"dnsNames": names,
			"issuerRef": map[string]any{
				"name":  issuerName,
				"kind":  issuerKind,
				"group": issuerGroup,
			},
		},
	}}
}
