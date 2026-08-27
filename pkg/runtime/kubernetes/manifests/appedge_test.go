package manifests

import (
	"strings"
	"testing"
)

func TestAppGatewayObjects(t *testing.T) {
	in := testInput(t)
	in.AppGateway = AppGatewayConfig{
		Namespace:      "donkey-system",
		ClassName:      "lgt-lb-standard",
		RouteNamespace: "projectcontour",
		IssuerName:     "lgt",
	}

	objects := NetworkObjects(in)
	if got := kindsOf(objects); got["ConfigMap"] != 1 || got["Certificate"] != 1 || got["Gateway"] != 1 || got["HTTPRoute"] != 1 {
		t.Fatalf("unexpected network objects: %v", got)
	}
	rendered, err := Render(objects)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"name: donkey-app-shop",
		"namespace: donkey-system",
		"secretName: donkey-app-shop-tls",
		"name: lgt",
		"kind: ClusterIssuer",
		"group: cert-manager.io",
		"gatewayClassName: lgt-lb-standard",
		"name: donkey-system-shop-gw",
		"namespace: projectcontour",
		"kubernetes.io/metadata.name: donkey-system",
		"- shop.acme.apps.example.com",
		"name: donkey-gateway",
		"port: 80",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("app edge should contain %q:\n%s", want, rendered)
		}
	}
	for _, constrained := range []string{"privateKey:", "duration:", "renewBefore:", "usages:"} {
		if strings.Contains(rendered, constrained) {
			t.Errorf("enterprise issuer policy should choose certificate %s", constrained)
		}
	}
}

func TestAppGatewayObjectsAreOptional(t *testing.T) {
	in := testInput(t)
	if got := kindsOf(NetworkObjects(in)); got["Gateway"] != 0 || got["Certificate"] != 0 || got["HTTPRoute"] != 0 {
		t.Fatalf("an unconfigured installation must not render app edge objects: %v", got)
	}

	in.AppGateway = AppGatewayConfig{Namespace: "donkey-system", ClassName: "lgt-lb-standard", IssuerName: "lgt"}
	in.Domain = ""
	if got := kindsOf(NetworkObjects(in)); got["Gateway"] != 0 || got["Certificate"] != 0 || got["HTTPRoute"] != 0 {
		t.Fatalf("an internal app must not render app edge objects: %v", got)
	}
}

func TestAppGatewayIssuerCanBeExternal(t *testing.T) {
	in := testInput(t)
	in.AppGateway = AppGatewayConfig{
		Namespace:   "donkey-system",
		ClassName:   "private-edge",
		IssuerName:  "vault-pki",
		IssuerKind:  "VaultClusterIssuer",
		IssuerGroup: "pki.example.com",
	}
	rendered, err := Render(AppGatewayObjects(in))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kind: VaultClusterIssuer", "group: pki.example.com"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("external issuer setting %q did not survive:\n%s", want, rendered)
		}
	}
}
