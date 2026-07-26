package manifests

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
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
	// The GatewayClass must carry donkey's own EnvoyProxy parameters: without
	// them Envoy Gateway defaults the entry service to LoadBalancer, which
	// never gets an address on clusters without a load-balancer controller —
	// leaving the Gateway unprogrammed and every hostname unroutable.
	for _, want := range []string{"kind: EnvoyProxy", "parametersRef", "type: LoadBalancer"} {
		if !strings.Contains(out, want) {
			t.Errorf("system objects should contain %s", want)
		}
	}
	local := systemInput()
	local.GatewayServiceType = "ClusterIP"
	if out := componentNames(local); !strings.Contains(out, "type: ClusterIP") {
		t.Error("gateway.serviceType must reach the EnvoyProxy manifest")
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

// A registry whose certificate comes from a private CA is common in-house;
// both the server and the build engine have to trust it, or every push fails.
func TestRegistryCAReachesBuildEngineAndServer(t *testing.T) {
	in := systemInput()
	in.TLSIssuer = "platform"
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
	plain.TLSIssuer = "platform"
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
	in.TLSIssuer = "platform"
	objs := SystemObjects(in)

	ownCluster := map[string]bool{
		"GatewayClass:" + GatewayClassName:     true,
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
		case SystemNamespace:
		case "":
			if !ownCluster[kind+":"+m.GetName()] && !(kind == "Namespace" && m.GetName() == SystemNamespace) {
				t.Errorf("%s %s is cluster-wide and not donkey's own", kind, m.GetName())
			}
		default:
			t.Errorf("%s %s belongs to namespace %q — donkey installs into %s only", kind, m.GetName(), ns, SystemNamespace)
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
