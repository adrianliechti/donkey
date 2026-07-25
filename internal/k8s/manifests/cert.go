package manifests

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

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
// its own namespace. The short hash keeps names unambiguous: dashes are legal
// inside space and app names, so (space "team-a", app "web") and (space
// "team", app "a-web") would otherwise share a certificate name — and
// destroying one app would take the other's certificate with it.
func AppCertName(space, app string) string {
	sum := sha256.Sum256([]byte(space + "/" + app))
	return fmt.Sprintf("donkey-tls-%s-%s-%s", space, app, hex.EncodeToString(sum[:3]))
}

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
