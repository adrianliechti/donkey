package manifests

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/adrianliechti/donkey/internal/config"
)

func systemInput() SystemInput {
	return SystemInput{
		Namespace:    DefaultSystemNamespace,
		ServerImage:  "reg.example.com/donkey-server:test",
		Domain:       "apps.example.com",
		RegistryURL:  "reg.example.com/donkey",
		SessionKey:   "sessionkey",
		TelemetryKey: "telemetrykey",
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

// The control plane is self-contained: donkey-gateway owns TLS, routing and
// login. The default ACME setup needs only built-in Kubernetes resources.
func TestSystemObjectsSelfContainedEdge(t *testing.T) {
	in := systemInput()
	in.OIDCIssuer = "https://login.microsoftonline.com/x/v2.0"
	out := componentNames(in)
	for _, want := range []string{"donkey-server", "donkey-buildkitd", "donkey-packages", "donkey-gateway", "donkey-telemetry", "telemetry.duckdb", "DONKEY_TLS_MODE", "type: LoadBalancer"} {
		if !strings.Contains(out, want) {
			t.Errorf("system objects should contain %s", want)
		}
	}
	local := systemInput()
	local.GatewayServiceType = "ClusterIP"
	if out := componentNames(local); !strings.Contains(out, "type: ClusterIP") {
		t.Error("gateway.serviceType must reach the donkey-gateway Service")
	}
	for _, object := range SystemObjects(in) {
		if _, optionalAPI := object.(*unstructured.Unstructured); optionalAPI {
			t.Errorf("default installation unexpectedly rendered optional API object %s", object.GetObjectKind().GroupVersionKind().Kind)
		}
	}
	// The OIDC settings reach the server and the gateway.
	if !strings.Contains(out, "DONKEY_OIDC_ISSUER") {
		t.Error("server must receive the OIDC issuer")
	}
}

// A registry whose certificate comes from a private CA is common in-house;
// the server, build engine and package proxy have to trust it.
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
		"DONKEY_PACKAGES_CA_FILE",        // package upstreams use the same enterprise CA
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

func TestRegistryCARotationRollsEveryTrustReader(t *testing.T) {
	first := systemInput()
	first.RegistryCASecret = "donkey-registry-ca"
	first.RegistryCAFingerprint = "first-ca"
	second := first
	second.RegistryCAFingerprint = "rotated-ca"

	for name, hashes := range map[string][2]string{
		"buildkit": {first.buildkitConfigHash(), second.buildkitConfigHash()},
		"packages": {first.packagesConfigHash(), second.packagesConfigHash()},
		"server":   {first.serverConfigHash(), second.serverConfigHash()},
	} {
		if hashes[0] == hashes[1] {
			t.Errorf("%s did not roll when ca.crt changed", name)
		}
	}
}

func TestRegistryCredentialsPullMirroredSystemImages(t *testing.T) {
	in := systemInput()
	in.RegistryUsername = "builder"
	in.RegistryPassword = "s3cret"
	in.BuildkitImage = "reg.example.com/mirror/moby/buildkit:v0.31.2"
	out := componentNames(in)
	if !strings.Contains(out, "name: "+SystemRegistrySecretName) || !strings.Contains(out, "imagePullSecrets") {
		t.Fatalf("credentialed registry should render and reference a system pull secret:\n%s", out)
	}
	if n := strings.Count(out, "name: "+SystemRegistrySecretName); n != 7 {
		t.Errorf("pull secret should be created once and referenced by six system deployments, found %d mentions", n)
	}

	plain := componentNames(systemInput())
	if strings.Contains(plain, SystemRegistrySecretName) || strings.Contains(plain, "imagePullSecrets") {
		t.Error("an anonymous installation should not render system image pull credentials")
	}
}

func TestRegistryDockerConfigSecretReachesServer(t *testing.T) {
	in := systemInput()
	in.RegistryDockerConfigSecret = "registry-mirror-auth"
	dep := serverDeployment(in)
	container := dep.Spec.Template.Spec.Containers[0]
	foundEnv, foundMount := false, false
	for _, env := range container.Env {
		if env.Name == "DONKEY_REGISTRY_CONFIG_DIR" && env.Value == registryDockerConfigPath {
			foundEnv = true
		}
	}
	for _, mounted := range container.VolumeMounts {
		if mounted.Name == "registry-auth" && mounted.MountPath == registryDockerConfigPath && mounted.ReadOnly {
			foundMount = true
		}
	}
	if !foundEnv || !foundMount {
		t.Fatalf("registry Docker config wiring: env=%v mount=%v", foundEnv, foundMount)
	}
	for _, volume := range dep.Spec.Template.Spec.Volumes {
		if volume.Name == "registry-auth" {
			if volume.Secret == nil || volume.Secret.SecretName != "registry-mirror-auth" {
				t.Fatalf("registry-auth volume = %+v", volume)
			}
			return
		}
	}
	t.Fatal("registry Docker config Secret has no volume")
}

func TestAddonImageOverridesReachServer(t *testing.T) {
	in := systemInput()
	in.AddonImages = config.InstallAddonImages{
		Postgres: "reg.example.com/mirror/library/postgres",
		RustFS:   "reg.example.com/mirror/rustfs/rustfs",
	}
	in.AddonImagePullSecrets = []string{"registry-example-com", "shared-auth"}
	out := componentNames(in)
	for _, want := range []string{
		"DONKEY_POSTGRES_IMAGE", in.AddonImages.Postgres,
		"DONKEY_RUSTFS_IMAGE", in.AddonImages.RustFS,
		"DONKEY_ADDON_IMAGE_PULL_SECRETS", "registry-example-com,shared-auth",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("addon image override did not reach donkey-server: missing %q", want)
		}
	}
}

func TestCertificateAndDNSSettingsReachServer(t *testing.T) {
	in := systemInput()
	in.TLS = config.InstallTLS{
		Mode:       "cert-manager",
		IssuerName: "platform-ca",
		IssuerKind: "ClusterIssuer", IssuerGroup: "cert-manager.io",
	}
	in.ExternalDNS = config.InstallExternalDNS{
		Targets: []string{"10.204.166.5"}, RecordType: "A", TTL: 300,
	}
	out := componentNames(in)
	for _, want := range []string{
		"DONKEY_TLS_MODE", "cert-manager",
		"DONKEY_TLS_ISSUER_NAME", "platform-ca",
		"DONKEY_TLS_ISSUER_KIND", "ClusterIssuer",
		"DONKEY_TLS_ISSUER_GROUP", "cert-manager.io",
		"DONKEY_EXTERNAL_DNS_TARGETS", "10.204.166.5",
		"DONKEY_EXTERNAL_DNS_RECORD_TYPE", "DONKEY_EXTERNAL_DNS_TTL",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("traffic setting did not reach donkey-server: missing %q", want)
		}
	}

	objects := SystemObjects(in)
	if got := kindsOf(objects); got["Certificate"] != 1 || got["DNSEndpoint"] != 1 {
		t.Fatalf("unexpected system traffic objects: %v", got)
	}
	out, err := Render(objects)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"name: donkey-gateway",
		"secretName: donkey-gateway-tls",
		"dnsName: apps.example.com",
		"recordType: A",
		"kind: ClusterIssuer",
		"group: cert-manager.io",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("system traffic objects should contain %q", want)
		}
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
		// donkey-ai reads the binding Secrets to know which per-space tokens
		// are valid; with a namespace per space those are spread across all
		// of them, so this one is read-only and cluster-scoped.
		"ClusterRole:" + AIAppName:        true,
		"ClusterRoleBinding:" + AIAppName: true,
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
		"donkey.local/backend: http://donkey-server.donkey-system.svc:80",
		"donkey.local/login: required",
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
	if strings.Contains(out, "donkey.local/access") {
		t.Error("empty admin rules must not render an access annotation")
	}

	restricted := systemInput()
	restricted.PlatformAdmins = &config.AccessConfig{Users: []string{"ada@example.com"}}
	out = componentNames(restricted)
	if strings.Contains(out, "donkey.local/access") || !strings.Contains(out, "DONKEY_PLATFORM_ADMINS") || !strings.Contains(out, "ada@example.com") {
		t.Error("admin seed must reach the server while the console admits space members")
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

// The console route is the one backend that verifies identity itself, so it
// is the one route that receives a credential. Every app route must not:
// an app handed a token could replay its visitors against the API.
func TestOnlyTheAPIRouteCarriesAToken(t *testing.T) {
	out := componentNames(systemInput())
	if !strings.Contains(out, "donkey.local/token: forward") {
		t.Error("the console route should be marked for token forwarding")
	}
	if n := strings.Count(out, "donkey.local/token"); n != 1 {
		t.Errorf("exactly one route may carry a credential, found %d", n)
	}

	in := testInput(t)
	objects := NetworkObjects(in)
	for _, obj := range objects {
		cm, ok := obj.(*corev1.ConfigMap)
		if !ok {
			continue
		}
		if _, marked := cm.Annotations[AnnotationToken]; marked {
			t.Errorf("app route %s must never be marked for token forwarding", cm.Name)
		}
	}
}

// Defence in depth behind the token check: app pods should not be able to
// reach the admin API at all, but the AI proxy shares its listener — so the
// policy admits apps and the credential check stops them.
func TestServerNetworkPolicy(t *testing.T) {
	out := componentNames(systemInput())
	if !strings.Contains(out, "name: donkey-server\n") || !strings.Contains(out, "kind: NetworkPolicy") {
		t.Error("donkey-server should be guarded by a NetworkPolicy")
	}
	if !strings.Contains(out, "port: 8088") {
		t.Error("the policy should name the API port")
	}
}
