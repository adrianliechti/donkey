package manifests

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/adrianliechti/donkey/internal/config"
)

func systemInput() SystemInput {
	return SystemInput{
		Namespace:   DefaultSystemNamespace,
		ServerImage: "reg.example.com/donkey-server:test",
		Domain:      "apps.example.com",
		RegistryURL: "reg.example.com/donkey",
		SessionKey:  "sessionkey",
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

// The control plane is self-contained: donkey-gateway is the edge (TLS,
// routing, login) — no Gateway API, no Envoy, no cert-manager, no Ingress,
// no bundled auth components.
func TestSystemObjectsSelfContainedEdge(t *testing.T) {
	in := systemInput()
	in.OIDCIssuer = "https://login.microsoftonline.com/x/v2.0"
	out := componentNames(in)
	for _, want := range []string{"donkey-server", "donkey-buildkitd", "donkey-gateway", "DONKEY_TLS_MODE", "type: LoadBalancer"} {
		if !strings.Contains(out, want) {
			t.Errorf("system objects should contain %s", want)
		}
	}
	local := systemInput()
	local.GatewayServiceType = "ClusterIP"
	if out := componentNames(local); !strings.Contains(out, "type: ClusterIP") {
		t.Error("gateway.serviceType must reach the donkey-gateway Service")
	}
	for _, forbidden := range []string{
		"kind: Ingress", "oauth2-proxy", "pocket-id", "nginx",
		"kind: Gateway", "kind: GatewayClass", "kind: HTTPRoute",
		"kind: EnvoyProxy", "kind: Certificate", "cert-manager",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("system objects must not contain %s", forbidden)
		}
	}
	// The OIDC settings reach the server and the gateway.
	if !strings.Contains(out, "DONKEY_OIDC_ISSUER") {
		t.Error("server must receive the OIDC issuer")
	}
}

// A registry whose certificate comes from a private CA is common in-house;
// both the server and the build engine have to trust it, or every push fails.
func TestRegistryCAReachesBuildEngineAndServer(t *testing.T) {
	in := systemInput()
	in.RegistryCASecret = "donkey-registry-ca"
	out, err := Render(SystemObjects(in))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"buildkitd.toml", // the config that names the CA
		`ca = ["/etc/donkey/registry-ca/ca.crt"]`,
		"--config",                       // buildkitd is told to read it
		"secretName: donkey-registry-ca", // …and the CA is mounted
		"DONKEY_REGISTRY_CA_FILE",        // the server trusts it too
	} {
		if !strings.Contains(out, want) {
			t.Errorf("an installation with a registry CA should render %s", want)
		}
	}

	// Without one, nothing of that machinery appears.
	plain := systemInput()
	out, err = Render(SystemObjects(plain))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "/etc/donkey/registry-ca") {
		t.Error("a publicly trusted registry needs no CA mount")
	}
}

// Installing donkey must stay inside donkey's own house: no edits to cluster
// services (CoreDNS and friends), no node-level changes, no webhooks that
// could affect other workloads. Everything donkey applies is either in
// donkey-system or a cluster-scoped object it names after itself.
func TestInstallTouchesOnlyItsOwnObjects(t *testing.T) {
	in := systemInput()
	objs := SystemObjects(in)

	ownCluster := map[string]bool{
		"ClusterRoleBinding:" + ServerName:     true,
		"ClusterRole:" + GatewayAppName:        true,
		"ClusterRoleBinding:" + GatewayAppName: true,
	}
	for _, obj := range objs {
		m, err := meta.Accessor(obj)
		if err != nil {
			t.Fatal(err)
		}
		kind := obj.GetObjectKind().GroupVersionKind().Kind
		switch ns := m.GetNamespace(); ns {
		case DefaultSystemNamespace:
		case "":
			if !ownCluster[kind+":"+m.GetName()] && !(kind == "Namespace" && m.GetName() == DefaultSystemNamespace) {
				t.Errorf("%s %s is cluster-wide and not donkey's own", kind, m.GetName())
			}
		default:
			t.Errorf("%s %s belongs to namespace %q — donkey installs into %s only", kind, m.GetName(), ns, DefaultSystemNamespace)
		}
	}

	rendered, err := Render(objs)
	if err != nil {
		t.Fatal(err)
	}
	// Cluster-wide machinery donkey has no business installing. (The demo
	// scaffolding in hack/ does patch CoreDNS and node files — that is why it
	// lives there and never in the product.)
	for _, forbidden := range []string{
		"kube-system", "kube-public", "coredns", "CoreDNS",
		"kind: DaemonSet", "hostPath", "hostNetwork", "hostPID",
		"MutatingWebhookConfiguration", "ValidatingWebhookConfiguration",
		"kind: StorageClass", "kind: PriorityClass",
	} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("the installation must not contain %q — that is cluster-wide or node-level state donkey doesn't own", forbidden)
		}
	}
}

// The API lives on the domain itself (serverHost can move it below), and
// the central OAuth callback always stays on the domain: the gateway serves
// /oauth2 itself and learns the domain for its cookie — and it always
// issues a certificate for both hostnames.
func TestServerHostAndCentralCallback(t *testing.T) {
	in := systemInput()
	if got := in.ServerHost(); got != "apps.example.com" {
		t.Errorf("the API should default to the domain itself, got %s", got)
	}
	out := componentNames(in)
	for _, want := range []string{
		AnnotationHost + ": apps.example.com", // the console's routing entry
		"name: donkey-gateway",
		"DONKEY_DOMAIN",      // the gateway knows the cookie/callback domain
		"DONKEY_SERVER_HOST", // …and which hostname the console lives on
	} {
		if !strings.Contains(out, want) {
			t.Errorf("system objects should contain %q", want)
		}
	}

	moved := systemInput()
	moved.ServerHostname = "console.apps.example.com"
	if got := moved.ServerHost(); got != "console.apps.example.com" {
		t.Errorf("serverHost must override the API hostname, got %s", got)
	}
	out = componentNames(moved)
	if !strings.Contains(out, "console.apps.example.com") {
		t.Error("the console route should serve the overridden hostname")
	}
}

// The console rides through the donkey gateway like any app: its route
// points at the gateway and carries the real backend and login annotations —
// plus the admin rules when set. The static API token is gone entirely.
func TestConsoleBehindGateway(t *testing.T) {
	in := systemInput()
	out := componentNames(in)
	for _, want := range []string{
		"donkey.dev/backend: http://donkey-server.donkey-system.svc.cluster.local:80",
		"donkey.dev/login: required",
		"DONKEY_SESSION_KEY", // the server verifies CLI tokens itself
	} {
		if !strings.Contains(out, want) {
			t.Errorf("system objects should contain %q", want)
		}
	}
	for _, forbidden := range []string{"api-token", "DONKEY_API_TOKEN"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("the static API token must be gone, found %q", forbidden)
		}
	}
	if strings.Contains(out, "donkey.dev/access") {
		t.Error("empty admin rules must not render an access annotation")
	}

	restricted := systemInput()
	restricted.Admins = &config.AccessConfig{Users: []string{"ada@example.com"}}
	out = componentNames(restricted)
	if !strings.Contains(out, "donkey.dev/access") || !strings.Contains(out, "ada@example.com") {
		t.Error("admin rules should reach the console route's access annotation")
	}
}

// The pull policy decides whether a pod may reuse a cached layer. Getting it
// wrong is silent in both directions: a stale binary under a moved tag, or a
// registry outage that blocks every restart.
func TestImagePullPolicy(t *testing.T) {
	for image, want := range map[string]corev1.PullPolicy{
		"registry.example.com/donkey/server:v1.2.3":     corev1.PullIfNotPresent,
		"registry.example.com/donkey/server@sha256:abc": corev1.PullIfNotPresent,
		"reg.example.com:5000/donkey/server:v1":         corev1.PullIfNotPresent,
		// Moving tags must always be re-pulled.
		"registry.example.com/donkey/server:latest":  corev1.PullAlways,
		"registry.platform/donkey/donkey-server:dev": corev1.PullAlways,
		"registry.example.com/donkey/server":         corev1.PullAlways,
		"reg.example.com:5000/donkey/server":         corev1.PullAlways,
	} {
		if got := imagePullPolicy(image); got != want {
			t.Errorf("imagePullPolicy(%q) = %v, want %v", image, got, want)
		}
	}
}
