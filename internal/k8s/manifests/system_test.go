package manifests

import (
	"strings"
	"testing"
)

func systemInput() SystemInput {
	return SystemInput{
		Namespace:   SystemNamespace,
		ServerImage: "reg.example.com/donkey-server:test",
		BaseDomain:  "apps.example.com",
		RegistryURL: "reg.example.com/donkey",
		APIToken:    "tok",
	}
}

func componentNames(in SystemInput) string {
	objs := SystemObjects(in)
	rendered, err := Render(objs)
	if err != nil {
		panic(err)
	}
	return rendered
}

// The control plane is Gateway API on Envoy — no Ingress, no nginx, no
// bundled auth components.
func TestSystemObjectsGatewayAPI(t *testing.T) {
	in := systemInput()
	in.OIDCIssuer = "https://login.microsoftonline.com/x/v2.0"
	out := componentNames(in)
	for _, want := range []string{"kind: GatewayClass", "kind: Gateway", "kind: HTTPRoute", "gateway.envoyproxy.io/gatewayclass-controller", "donkey-server", "donkey-buildkitd"} {
		if !strings.Contains(out, want) {
			t.Errorf("system objects should contain %s", want)
		}
	}
	for _, forbidden := range []string{"kind: Ingress", "oauth2-proxy", "pocket-id", "nginx"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("system objects must not contain %s", forbidden)
		}
	}
	// The OIDC settings reach the server (which renders per-app policies).
	if !strings.Contains(out, "DONKEY_OIDC_ISSUER") {
		t.Error("server must receive the OIDC issuer")
	}
}
