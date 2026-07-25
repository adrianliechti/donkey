package manifests

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TLSConfig points at the cert-manager issuer that signs app certificates.
// donkey requires cert-manager whenever it serves https: spaces put apps one
// DNS level below the base domain, where a single wildcard cannot reach them,
// and renewing certificates is not something a PaaS should hand-roll.
type TLSConfig struct {
	// Issuer is the cert-manager issuer's name, Kind either ClusterIssuer
	// (the usual case) or Issuer.
	Issuer string
	Kind   string
}

// AppCertName is the Certificate (and Secret) for one app's hostname. It
// lives next to the Gateway, because a listener may only reference secrets in
// its own namespace.
func AppCertName(space, app string) string { return "donkey-tls-" + space + "-" + app }

// appCertificate asks cert-manager for the app's certificate. One certificate
// per hostname keeps donkey compatible with every solver — HTTP-01 included,
// which a wildcard could never use.
func appCertificate(in Input) *unstructured.Unstructured {
	kind := in.TLS.Kind
	if kind == "" {
		kind = "ClusterIssuer"
	}
	name := AppCertName(in.Space, in.App.Name)
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]any{
			"name":      name,
			"namespace": in.GatewayNamespace,
			"labels":    toAnyMap(Labels(in.Space, in.App.Name, ComponentWeb)),
		},
		"spec": map[string]any{
			"secretName": name,
			"dnsNames":   []any{in.Domain},
			"issuerRef": map[string]any{
				"name":  in.TLS.Issuer,
				"kind":  kind,
				"group": "cert-manager.io",
			},
		},
	}}
}
