package manifests

import (
	"strings"
	"testing"
)

func TestAppGatewayObjects(t *testing.T) {
	in := testInput(t)
	in.AppGateway = AppGatewayConfig{
		Namespace:      "donkey-system",
		ClassName:      "platform",
		RouteNamespace: "projectcontour",
		IssuerName:     "platform-ca",
	}

	objects, err := NetworkObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := kindsOf(objects); got["ConfigMap"] != 1 || got["Certificate"] != 1 || got["Gateway"] != 1 || got["HTTPRoute"] != 1 {
		t.Fatalf("unexpected network objects: %v", got)
	}
	rendered, err := Render(objects)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"name: donkey-app-acme.shop",
		"namespace: donkey-system",
		"secretName: donkey-app-acme.shop-tls",
		"name: platform-ca",
		"kind: ClusterIssuer",
		"group: cert-manager.io",
		"gatewayClassName: platform",
		"name: shop",
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

func TestContourHTTPProxyObjects(t *testing.T) {
	in := testInput(t)
	in.ContourHTTPProxy = ContourHTTPProxyConfig{
		Namespace:        "donkey-system",
		IngressClassName: "contour-private",
		IssuerName:       "platform-ca",
		IssuerKind:       "ClusterIssuer",
		IssuerGroup:      "cert-manager.io",
	}

	objects, err := NetworkObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := kindsOf(objects); got["ConfigMap"] != 1 || got["Certificate"] != 1 || got["HTTPProxy"] != 1 || got["Gateway"] != 0 || got["HTTPRoute"] != 0 {
		t.Fatalf("unexpected Contour network objects: %v", got)
	}
	rendered, err := Render(objects)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"apiVersion: projectcontour.io/v1",
		"kind: HTTPProxy",
		"namespace: donkey-system",
		"ingressClassName: contour-private",
		"fqdn: shop.acme.apps.example.com",
		"secretName: donkey-app-acme.shop-tls",
		"enableWebsockets: true",
		"response: 600s",
		"idle: 600s",
		"name: donkey-gateway",
		"port: 80",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("Contour edge should contain %q:\n%s", want, rendered)
		}
	}
}

func TestAppEdgeStrategiesAreExclusive(t *testing.T) {
	in := testInput(t)
	in.AppGateway = AppGatewayConfig{Namespace: "donkey-system", ClassName: "platform", IssuerName: "platform-ca"}
	in.ContourHTTPProxy = ContourHTTPProxyConfig{Namespace: "donkey-system", IngressClassName: "contour", IssuerName: "platform-ca"}
	if _, err := NetworkObjects(in); err == nil || !strings.Contains(err.Error(), "cannot both expose") {
		t.Fatalf("two per-host edge strategies should fail clearly, got %v", err)
	}
}

func TestAppEdgeNamesIncludeLogicalSpace(t *testing.T) {
	first := AppEdgeName("alpha", "shop")
	second := AppEdgeName("beta", "shop")
	if first == second {
		t.Fatalf("two spaces produced the same app edge name %q", first)
	}
	// The delimiter must be unambiguous even when logical names contain
	// dashes; these pairs collide under ordinary dash-joining.
	if AppEdgeName("a", "b-c") == AppEdgeName("a-b", "c") {
		t.Fatal("edge name is ambiguous across space/app pairs")
	}
}

func TestAppGatewayObjectsAreOptional(t *testing.T) {
	in := testInput(t)
	objects, err := NetworkObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := kindsOf(objects); got["Gateway"] != 0 || got["Certificate"] != 0 || got["HTTPRoute"] != 0 || got["HTTPProxy"] != 0 {
		t.Fatalf("an unconfigured installation must not render app edge objects: %v", got)
	}

	in.AppGateway = AppGatewayConfig{Namespace: "donkey-system", ClassName: "platform", IssuerName: "platform-ca"}
	in.Domain = ""
	objects, err = NetworkObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := kindsOf(objects); got["Gateway"] != 0 || got["Certificate"] != 0 || got["HTTPRoute"] != 0 || got["HTTPProxy"] != 0 {
		t.Fatalf("an internal app must not render app edge objects: %v", got)
	}

	in.AppGateway = AppGatewayConfig{}
	in.ContourHTTPProxy = ContourHTTPProxyConfig{Namespace: "donkey-system", IngressClassName: "contour", IssuerName: "platform-ca"}
	objects, err = NetworkObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := kindsOf(objects); got["Certificate"] != 0 || got["HTTPProxy"] != 0 {
		t.Fatalf("an internal app must not render a Contour edge: %v", got)
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
	objects, err := AppGatewayObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := Render(objects)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kind: VaultClusterIssuer", "group: pki.example.com"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("external issuer setting %q did not survive:\n%s", want, rendered)
		}
	}
}

func TestAppGatewayNameTemplate(t *testing.T) {
	in := testInput(t)
	in.AppGateway = AppGatewayConfig{
		Namespace:    "lgt-donkey-platform",
		ClassName:    "platform",
		NameTemplate: "$namespace-$hash",
		IssuerName:   "platform-ca",
	}

	objects, err := AppGatewayObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := Render(objects)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, "name: lgt-donkey-platform-") {
		t.Fatalf("configured Gateway name template was not expanded:\n%s", rendered)
	}
	if strings.Contains(rendered, "lgt-donkey-platform.donkey-app-") {
		t.Fatalf("legacy dotted Gateway name survived:\n%s", rendered)
	}
}
