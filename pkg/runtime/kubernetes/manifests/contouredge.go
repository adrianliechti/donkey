package manifests

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	// ContourSystemEdgeName groups the Certificate and HTTPProxies that make
	// the console and central OAuth callback reachable through Contour.
	ContourSystemEdgeName        = "donkey-contour-edge"
	ContourSystemEdgeSecretName  = ContourSystemEdgeName + "-tls"
	ContourSystemServerProxyName = ContourSystemEdgeName + "-server"
)

// ContourHTTPProxyConfig provisions exact-host virtual hosts on one shared
// Contour data plane. The shared Gateway is platform-owned; it needs one
// projectcontour.io/https listener, but donkey never mutates it.
type ContourHTTPProxyConfig struct {
	Namespace        string
	IngressClassName string
	IssuerName       string
	IssuerKind       string
	IssuerGroup      string
}

func (c ContourHTTPProxyConfig) Enabled() bool { return c.IngressClassName != "" }

// ContourHTTPProxyObjects returns the Certificate and root HTTPProxy for one
// app hostname. Both live next to donkey-gateway, keeping Secret and Service
// references same-namespace and avoiding delegation objects.
func ContourHTTPProxyObjects(in Input) []runtime.Object {
	if !in.ContourHTTPProxy.Enabled() || in.Domain == "" {
		return nil
	}
	name := AppEdgeName(in.Space, in.App.Name)
	secretName := AppCertificateSecretName(in.Space, in.App.Name)
	labels := Labels(in.Space, in.App.Name, RoleEdge)
	certificate := managedCertificate(name, in.ContourHTTPProxy.Namespace, secretName,
		[]string{in.Domain}, labels, in.ContourHTTPProxy.IssuerName,
		in.ContourHTTPProxy.IssuerKind, in.ContourHTTPProxy.IssuerGroup)
	proxy := contourHTTPProxy(name, in.ContourHTTPProxy.Namespace, in.Domain,
		secretName, in.ContourHTTPProxy.IngressClassName, labels)
	return []runtime.Object{certificate, proxy}
}

// contourSystemEdgeObjects exposes both places donkey's control plane may
// need: ServerHost for the console/API and Domain for the centralized OAuth
// callback. One SAN certificate backs both exact-host HTTPProxies.
func contourSystemEdgeObjects(in SystemInput) []runtime.Object {
	if !in.ContourHTTPProxy.Enabled() || in.Domain == "" {
		return nil
	}
	hosts := []string{in.Domain}
	if serverHost := in.ServerHost(); serverHost != in.Domain {
		hosts = append(hosts, serverHost)
	}
	labels := SystemLabels(ContourSystemEdgeName)
	certificate := managedCertificate(ContourSystemEdgeName, in.Namespace,
		ContourSystemEdgeSecretName, hosts, labels, in.ContourHTTPProxy.IssuerName,
		in.ContourHTTPProxy.CertificateIssuerKind(), in.ContourHTTPProxy.CertificateIssuerGroup())
	objects := []runtime.Object{certificate,
		contourHTTPProxy(ContourSystemEdgeName, in.Namespace, in.Domain,
			ContourSystemEdgeSecretName, in.ContourHTTPProxy.IngressClassName, labels),
	}
	if len(hosts) > 1 {
		objects = append(objects, contourHTTPProxy(ContourSystemServerProxyName, in.Namespace,
			hosts[1], ContourSystemEdgeSecretName, in.ContourHTTPProxy.IngressClassName, labels))
	}
	return objects
}

func contourHTTPProxy(name, namespace, hostname, secretName, ingressClassName string, labels map[string]string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "projectcontour.io/v1",
		"kind":       "HTTPProxy",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    toAnyMap(labels),
		},
		"spec": map[string]any{
			"ingressClassName": ingressClassName,
			"virtualhost": map[string]any{
				"fqdn": hostname,
				"tls": map[string]any{
					"secretName": secretName,
				},
			},
			"routes": []any{
				map[string]any{
					// Contour requires this switch for WebSocket upgrades; donkey
					// serves arbitrary apps, so the edge cannot guess which need it.
					"enableWebsockets": true,
					"timeoutPolicy": map[string]any{
						"response": "600s",
						"idle":     "600s",
					},
					"services": []any{
						map[string]any{"name": GatewayAppName, "port": int64(80)},
					},
				},
			},
		},
	}}
}
