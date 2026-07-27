package manifests

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/adrianliechti/donkey/internal/config"
)

// SystemInput describes the donkey control plane (namespace donkey-system):
// the API server, the in-cluster BuildKit and the donkey gateway — donkey's
// own edge proxy, which terminates TLS, routes every app and runs the OIDC
// login where a route demands one.
type SystemInput struct {
	Namespace   string // "donkey-system"
	ServerImage string
	Domain      string

	// ServerHostname overrides where the API and console live (default: the
	// domain itself). It must be the domain or a name under it — the login
	// cookie's domain has to cover it.
	ServerHostname string

	// TLSMode is how the gateway gets its certificates: "acme" (Let's
	// Encrypt, the default), "ca" (a self-signed CA the gateway mints and
	// signs per-hostname certificates with — local and internal installs),
	// or "static" (a wildcard certificate provided in TLSSecret).
	TLSMode string
	// ACMEEmail is the ACME account's contact (expiry warnings); optional.
	ACMEEmail string
	// ACMEDirectory overrides the ACME endpoint (default: Let's Encrypt
	// production) — staging, or an internal ACME CA.
	ACMEDirectory string
	// TLSSecret names a kubernetes.io/tls Secret in donkey-system holding
	// the certificate for static mode (default "donkey-gateway-tls").
	TLSSecret string

	// GatewayServiceType is the type of the service traffic enters the
	// cluster through: LoadBalancer (the default), NodePort or ClusterIP.
	// Local clusters without a load-balancer controller (kind) need
	// ClusterIP or NodePort — a LoadBalancer service never gets an address
	// there, which would leave every donkey hostname unroutable.
	GatewayServiceType string
	// GatewayHTTPNodePort/GatewayHTTPSNodePort pin the node ports of a
	// NodePort service, so port mappings (kind) or firewall rules can rely
	// on them.
	GatewayHTTPNodePort  int32
	GatewayHTTPSNodePort int32

	// RegistryCASecret names a secret in donkey-system holding ca.crt — the
	// certificate authority the registry's own certificate comes from. Both
	// the server and the build engine trust it.
	RegistryCASecret string

	RegistryURL      string
	RegistryUsername string
	RegistryPassword string
	RegistryInsecure bool
	// RegistryImmutable marks a registry that refuses to overwrite a tag, so
	// donkey makes every image tag unique.
	RegistryImmutable bool

	// Admins narrows who may use the console and CLI (users/groups/roles,
	// matched like an app's access rules). Empty means everyone the IdP
	// admits — the login itself is never optional for the control plane.
	Admins *config.AccessConfig

	OIDCIssuer       string // OIDC issuer of the login gateway
	OIDCClientID     string
	OIDCClientSecret string
	// SessionKey seals the login gateway's session cookies (32 bytes, hex).
	// Generated at install, reused on re-installs — rotating it signs
	// everyone out.
	SessionKey string

	OpenAIBaseURL string
	OpenAIAPIKey  string
	AIModel       string

	// DefaultPlatform is the build platform of the cluster's nodes.
	DefaultPlatform string

	// Build is how this installation builds apps: which builder an app with
	// no Dockerfile gets, the mirrors every build pulls through, and the
	// proxy donkey's own processes use.
	Build config.InstallBuild

	BuildkitImage string

	NamespacePrefix string
}

const (
	SystemNamespace = "donkey-system"
	// ServerName is donkey-server's Service — the openai addon points app
	// bindings at its AI proxy path.
	ServerName   = "donkey-server"
	buildkitName = "donkey-buildkitd"
	serverPort   = 8088
	buildkitPort = 1234

	// systemRunAsUser is the unprivileged identity donkey's own pods run as.
	// The image says nothing about a user (see hack/Dockerfile.server) —
	// this is where that is decided, because only the runtime can enforce
	// it. Everything the server writes goes to /tmp, so any uid works.
	systemRunAsUser = int64(1000)

	// systemPriorityClass keeps the control plane ahead of the apps it runs:
	// when a node runs short, the kubelet evicts the lowest priority first,
	// and donkey-server going down takes every deploy and log stream with it.
	// Kubernetes ships this class in every cluster (and allows it outside
	// kube-system since 1.17), so donkey installs no PriorityClass of its own.
	systemPriorityClass = "system-cluster-critical"
)

// Scheme is always https: apps sit behind an OIDC login whose cookies are
// Secure-only, so plain http was never a real option.
func (in SystemInput) Scheme() string { return "https" }

// ServerHost is where the donkey API and console live — the domain itself,
// unless overridden.
func (in SystemInput) ServerHost() string {
	if in.ServerHostname != "" {
		return in.ServerHostname
	}
	return in.Domain
}

// SystemObjects renders the whole control plane, in apply order.
func SystemObjects(in SystemInput) []runtime.Object {
	objs := []runtime.Object{
		systemNamespace(in),
		serverServiceAccount(in),
		serverClusterRoleBinding(in),
		systemSecret(in),
		// The build engine's config file has to exist before the deployment
		// that mounts it.
		buildkitConfig(in),
	}
	objs = append(objs,
		buildkitDeployment(in),
		systemService(in, buildkitName, buildkitPort, buildkitPort),
		buildkitNetworkPolicy(in),
		serverDeployment(in),
		systemService(in, ServerName, 80, serverPort),
		consoleRoute(in),
	)
	// The donkey gateway is the app edge — always there, login or not.
	return append(objs, donkeyGatewayObjects(in)...)
}

// configHash changes whenever the given configuration values change, so pods
// that read them only at startup get rolled on re-install. Each deployment
// hashes just what it consumes: rotating the session key must not kill in-flight
// builds, and a new registry host must roll buildkitd (which reads its config
// file once, at startup).
func configHash(values ...string) string {
	h := sha256.New()
	for _, v := range values {
		fmt.Fprintln(h, v)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// serverConfigHash covers the secret-backed env vars of donkey-server
// (secretKeyRef values are only read at pod creation).
func (in SystemInput) serverConfigHash() string {
	return configHash(in.SessionKey, in.OIDCClientID, in.OIDCClientSecret, in.RegistryPassword, in.OpenAIAPIKey,
		buildEnvJSON(in.Build), in.Build.Proxy.HTTP, in.Build.Proxy.HTTPS)
}

// buildkitConfigHash covers what buildkitd reads at startup: the config file
// itself, which it reads once and never again.
func (in SystemInput) buildkitConfigHash() string {
	// The proxy is secret-backed env, which a pod also only reads at
	// creation — so it belongs in the same hash as the config file.
	return configHash(buildkitTOML(in), in.Build.Proxy.HTTP, in.Build.Proxy.HTTPS, in.Build.Proxy.NoProxy)
}

func (in SystemInput) hashedTemplateMeta(name, hash string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Labels:      systemLabels(name),
		Annotations: map[string]string{"donkey.dev/config-hash": hash},
	}
}

// systemPodSecurityContext pins who donkey's own pods run as. The kubelet
// checks runAsNonRoot against this numeric uid, so an image without a USER
// still cannot come up as root.
func systemPodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot: new(true),
		RunAsUser:    new(systemRunAsUser),
		RunAsGroup:   new(systemRunAsUser),
	}
}

// systemSecurityContext is what makes that uid mean something: no way to
// gain more than it started with. donkey-server holds cluster-admin, so a
// foothold inside it is the worst one donkey has to offer. The uid itself
// is set once, on the pod, so it covers every container in it.
func systemSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             new(true),
		AllowPrivilegeEscalation: new(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func systemLabels(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       name,
		"app.kubernetes.io/managed-by": ManagedByValue,
		LabelApp:                       "donkey-system",
		LabelComponent:                 name,
	}
}

func systemNamespace(in SystemInput) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        in.Namespace,
			Labels:      systemLabels("donkey-system"),
			Annotations: map[string]string{AnnotationManaged: "true"},
		},
	}
}

func serverServiceAccount(in SystemInput) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: ServerName, Namespace: in.Namespace, Labels: systemLabels(ServerName)},
	}
}

// serverClusterRoleBinding grants cluster-admin for the PoC: donkey-server
// creates namespaces and everything inside them. A scoped ClusterRole is the
// production follow-up.
func serverClusterRoleBinding(in SystemInput) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: ServerName, Labels: systemLabels(ServerName)},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: ServerName, Namespace: in.Namespace}},
	}
}

func systemSecret(in SystemInput) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: "donkey-system-config", Namespace: in.Namespace, Labels: systemLabels(ServerName)},
		StringData: map[string]string{
			"registry-password":  in.RegistryPassword,
			"openai-api-key":     in.OpenAIAPIKey,
			"oidc-client-id":     in.OIDCClientID,
			"oidc-client-secret": in.OIDCClientSecret,
			"session-key":        in.SessionKey,
			// The build environment and the proxy routinely carry the
			// credentials of a package registry or the proxy itself
			// ("https://ci:token@jfrog.corp/..."), so they belong here with
			// the other secrets rather than in a Deployment anyone with read
			// access can print.
			"build-env":   buildEnvJSON(in.Build),
			"http-proxy":  in.Build.Proxy.HTTP,
			"https-proxy": in.Build.Proxy.HTTPS,
		},
	}
}

// registryCAPath is where both the server and buildkitd find the registry's
// certificate authority.
const registryCAPath = "/etc/donkey/registry-ca"

const (
	// buildkitConfigPath is where buildkitd finds its config file.
	buildkitConfigPath = "/etc/buildkit"

	// buildkitCachePath is buildkitd's state directory — the build cache. It
	// gets a volume of its own, bounded by buildkitCacheSize.
	buildkitCachePath = "/var/lib/buildkit"
	buildkitCacheSize = "10Gi"
	// buildkitCacheKeep is what the engine's collector never reclaims;
	// above buildkitCacheMax it collects. Both stay below the volume's size:
	// collection is asynchronous, so the cache must be able to overshoot its
	// target without the kubelet evicting the pod that holds it.
	buildkitCacheKeep = "5GB"
	buildkitCacheMax  = "8GB"
)

// BuildkitConfig is everything donkey expresses in the build engine's config
// file. Both runtimes render it from here: an installation behind a mirror
// must reach the same registries whether its apps run on Kubernetes or on a
// Docker engine.
type BuildkitConfig struct {
	// RegistryHost is donkey's own registry (host[:port], no path).
	RegistryHost string
	// RegistryCAFile is the certificate authority the registry's own
	// certificate comes from; empty when it is publicly trusted.
	RegistryCAFile string
	// RegistryPlainHTTP marks a registry donkey talks to over http.
	RegistryPlainHTTP bool
	// Mirrors maps an upstream registry host to the mirror serving it.
	Mirrors map[string]string
}

// registrySection is one [registry."host"] table. Hosts are collected before
// anything is rendered because a host may pick up settings from two places —
// donkey's registry and a mirror can be the same JFrog — and TOML rejects a
// table declared twice.
type registrySection struct {
	mirrors   []string
	caFile    string
	plainHTTP bool
}

// BuildkitTOML renders the build engine's config file. Every installation
// gets one, for the cache bound alone: with no garbage collection the build
// cache grows until the node's disk is full. A registry whose certificate is
// signed by a private CA is named here too — otherwise every push would fail
// verification — and so is every mirror, which is what makes builds work on a
// network whose only egress is one.
func BuildkitTOML(c BuildkitConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[worker.oci]\n  gc = true\n  reservedSpace = %q\n  maxUsedSpace = %q\n",
		buildkitCacheKeep, buildkitCacheMax)

	sections := map[string]*registrySection{}
	at := func(host string) *registrySection {
		if s, ok := sections[host]; ok {
			return s
		}
		s := &registrySection{}
		sections[host] = s
		return s
	}
	if c.RegistryHost != "" && (c.RegistryCAFile != "" || c.RegistryPlainHTTP) {
		s := at(c.RegistryHost)
		s.caFile = c.RegistryCAFile
		s.plainHTTP = c.RegistryPlainHTTP
	}
	for upstream, mirror := range c.Mirrors {
		host, plainHTTP := mirrorHost(mirror)
		// The key is a registry host and nothing else: BuildKit looks it up
		// by bare hostname, so a key written as a URL would match nothing and
		// every pull would quietly go to the real registry — the one thing a
		// mirrored installation has no route to.
		upstream, _ = mirrorHost(upstream)
		upstream = mirrorHostOnly(upstream)
		if upstream == "" || host == "" {
			continue
		}
		up := at(upstream)
		up.mirrors = append(up.mirrors, host)
		// The mirror is a registry in its own right: donkey reaches it over
		// the scheme it was given, and an in-house one usually sits behind
		// the same private CA as donkey's registry.
		m := at(mirrorHostOnly(host))
		m.plainHTTP = m.plainHTTP || plainHTTP
		if c.RegistryCAFile != "" && !plainHTTP {
			m.caFile = c.RegistryCAFile
		}
	}

	for _, host := range slices.Sorted(maps.Keys(sections)) {
		s := sections[host]
		if len(s.mirrors) == 0 && s.caFile == "" && !s.plainHTTP {
			continue
		}
		fmt.Fprintf(&b, "\n[registry.%q]\n", host)
		if len(s.mirrors) > 0 {
			slices.Sort(s.mirrors)
			quoted := make([]string, len(s.mirrors))
			for i, m := range s.mirrors {
				quoted[i] = strconv.Quote(m)
			}
			fmt.Fprintf(&b, "  mirrors = [%s]\n", strings.Join(quoted, ", "))
		}
		if s.caFile != "" {
			fmt.Fprintf(&b, "  ca = [%q]\n", s.caFile)
		}
		if s.plainHTTP {
			fmt.Fprintf(&b, "  http = true\n")
		}
	}
	return b.String()
}

// mirrorHost turns a configured mirror into what BuildKit's `mirrors` list
// wants: a host, optionally followed by a path prefix — never a URL. A
// scheme is accepted (an admin who writes one is not wrong, and JFrog's own
// UI hands out URLs) and answers the other question instead, whether the
// mirror is reached over plain http.
func mirrorHost(mirror string) (host string, plainHTTP bool) {
	rest := strings.TrimSpace(mirror)
	if s, after, ok := strings.Cut(rest, "://"); ok {
		plainHTTP = strings.EqualFold(s, "http")
		rest = after
	}
	return strings.Trim(rest, "/"), plainHTTP
}

// mirrorHostOnly drops the path prefix: TLS and CA settings belong to the
// host, not to the repository path under it.
func mirrorHostOnly(hostAndPath string) string {
	host, _, _ := strings.Cut(hostAndPath, "/")
	return host
}

// registryHostOf is the host part of donkey's registry setting, which is a
// prefix ("registry.example.com/donkey") rather than a bare host.
func registryHostOf(registryURL string) string {
	host, _ := mirrorHost(registryURL)
	return mirrorHostOnly(host)
}

func buildkitTOML(in SystemInput) string {
	host, _, _ := strings.Cut(in.RegistryURL, "/")
	// A registry declared insecure is named as such here, not only on the
	// export: the buildpacks builder makes the engine *pull* from donkey's
	// own registry too (a mirrored builder image lives there), and a pull
	// carries no per-solve attribute to fall back on.
	c := BuildkitConfig{
		RegistryHost:      host,
		RegistryPlainHTTP: in.RegistryInsecure,
		Mirrors:           in.Build.RegistryMirrors,
	}
	if in.RegistryCASecret != "" {
		c.RegistryCAFile = registryCAPath + "/ca.crt"
	}
	return BuildkitTOML(c)
}

func buildkitConfig(in SystemInput) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: buildkitName, Namespace: in.Namespace, Labels: systemLabels(buildkitName)},
		Data:       map[string]string{"buildkitd.toml": buildkitTOML(in)},
	}
}

func buildkitDeployment(in SystemInput) *appsv1.Deployment {
	labels := systemLabels(buildkitName)
	replicas := int32(1)
	privileged := true
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: buildkitName, Namespace: in.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				// The config hash rolls buildkitd when the registry setup changes.
				ObjectMeta: in.hashedTemplateMeta(buildkitName, in.buildkitConfigHash()),
				Spec:       buildkitPodSpec(in, privileged),
			},
		},
	}
}

func serverDeployment(in SystemInput) *appsv1.Deployment {
	labels := systemLabels(ServerName)
	replicas := int32(1)
	secretEnv := func(name, key string) corev1.EnvVar {
		return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "donkey-system-config"},
				Key:                  key,
			},
		}}
	}
	env := []corev1.EnvVar{
		{Name: "DONKEY_LISTEN", Value: fmt.Sprintf(":%d", serverPort)},
		{Name: "DONKEY_REGISTRY_URL", Value: in.RegistryURL},
		{Name: "DONKEY_REGISTRY_USERNAME", Value: in.RegistryUsername},
		{Name: "DONKEY_REGISTRY_INSECURE", Value: strconv.FormatBool(in.RegistryInsecure)},
		{Name: "DONKEY_REGISTRY_IMMUTABLE", Value: strconv.FormatBool(in.RegistryImmutable)},
		{Name: "DONKEY_REGISTRY_CA_FILE", Value: registryCAFile(in)},
		{Name: "DONKEY_DOMAIN", Value: in.Domain},
		{Name: "DONKEY_BUILDKIT_ADDR", Value: fmt.Sprintf("tcp://%s.%s.svc.cluster.local:%d", buildkitName, in.Namespace, buildkitPort)},
		{Name: "DONKEY_NAMESPACE_PREFIX", Value: in.NamespacePrefix},
		{Name: "DONKEY_DEFAULT_PLATFORM", Value: in.DefaultPlatform},
		{Name: "DONKEY_OIDC_ISSUER", Value: in.OIDCIssuer},
		secretEnv("DONKEY_OIDC_CLIENT_ID", "oidc-client-id"),
		secretEnv("DONKEY_OIDC_CLIENT_SECRET", "oidc-client-secret"),
		{Name: "DONKEY_AI_MODEL", Value: in.AIModel},
		{Name: "OPENAI_BASE_URL", Value: in.OpenAIBaseURL},
		// The session key lets the server verify sealed CLI tokens itself —
		// the direct (port-forwarded) path next to the gateway's headers.
		secretEnv("DONKEY_SESSION_KEY", "session-key"),
		{Name: "DONKEY_ADMINS", Value: adminsJSON(in)},
		secretEnv("DONKEY_REGISTRY_PASSWORD", "registry-password"),
		secretEnv("OPENAI_API_KEY", "openai-api-key"),
	}
	env = append(env, buildEnv(in.Build)...)
	// The server resolves versions and image configuration itself, so it
	// needs the proxy as much as the build engine does.
	env = append(env, proxyEnv(in.Build.Proxy, registryHostOf(in.RegistryURL))...)
	health := corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(serverPort)},
	}
	spec := corev1.PodSpec{
		ServiceAccountName: ServerName,
		PriorityClassName:  systemPriorityClass,
		SecurityContext:    systemPodSecurityContext(),
		Containers: []corev1.Container{{
			Name:            "server",
			ImagePullPolicy: imagePullPolicy(in.ServerImage),
			Image:           in.ServerImage,
			Env:             env,
			Ports:           []corev1.ContainerPort{{ContainerPort: serverPort}},
			Resources:       serverResources(),
			SecurityContext: systemSecurityContext(),
			// Readiness asks /readyz, which starts failing the moment the pod
			// begins draining — that is what takes it out of the Service while
			// it finishes the deploys it already accepted.
			ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(serverPort)},
			}},
			// A wedged server takes every deploy and log stream with it, and
			// nobody is watching it the way they watch their apps. /healthz
			// keeps answering while the server drains, so this never cuts a
			// shutdown short.
			LivenessProbe: &corev1.Probe{ProbeHandler: health, PeriodSeconds: 15, FailureThreshold: 3},
		}},
	}
	if in.RegistryCASecret != "" {
		spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
			{Name: "registry-ca", MountPath: registryCAPath, ReadOnly: true},
		}
		spec.Volumes = []corev1.Volume{{
			Name: "registry-ca",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: in.RegistryCASecret},
			},
		}}
	}
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: ServerName, Namespace: in.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: in.hashedTemplateMeta(ServerName, in.serverConfigHash()),
				Spec:       spec,
			},
		},
	}
}

// buildEnv is the installation's build configuration as donkey-server reads
// it. Only the settings the server acts on travel here: the mirrors are the
// build engine's business, and the proxy is spelled the way every tool
// already expects (see proxyEnv).
func buildEnv(b config.InstallBuild) []corev1.EnvVar {
	var env []corev1.EnvVar
	// Only what was configured: an installation that says nothing about
	// builds renders the deployment it always did.
	for _, e := range []corev1.EnvVar{
		{Name: "DONKEY_BUILD_BUILDER", Value: string(b.Builder)},
		{Name: "DONKEY_BUILD_BUILDPACKS_BUILDER_IMAGE", Value: b.Buildpacks.BuilderImage},
		{Name: "DONKEY_BUILD_BUILDPACKS_RUN_IMAGE", Value: b.Buildpacks.RunImage},
		{Name: "DONKEY_BUILD_BUILDPACKS_PLATFORM_API", Value: b.Buildpacks.PlatformAPI},
		{Name: "DONKEY_BUILD_RAILPACK_BUILDER_IMAGE", Value: b.Railpack.BuilderImage},
		{Name: "DONKEY_BUILD_RAILPACK_RUNTIME_IMAGE", Value: b.Railpack.RuntimeImage},
	} {
		if e.Value != "" {
			env = append(env, e)
		}
	}
	if len(b.Env) > 0 {
		// A map travels as JSON, the way DONKEY_ADMINS does — but out of the
		// Secret, because these values are routinely credentialed URLs.
		env = append(env, secretEnvVar("DONKEY_BUILD_ENV", "build-env"))
	}
	return env
}

// buildEnvJSON is the build environment as donkey-server reads it.
func buildEnvJSON(b config.InstallBuild) string {
	if len(b.Env) == 0 {
		return ""
	}
	raw, err := json.Marshal(b.Env)
	if err != nil {
		return ""
	}
	return string(raw)
}

// secretEnvVar reads one value out of the installation's system Secret.
func secretEnvVar(name, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "donkey-system-config"},
			Key:                  key,
		},
	}}
}

// inClusterNoProxy is what must never be sent to a forward proxy: donkey's
// own components reach each other over cluster addresses, and no corporate
// proxy can route those. It is appended to whatever the installation
// configured, because forgetting it does not degrade anything — it takes the
// control plane off the air.
//
// The Kubernetes API is not in this list because it cannot be: a pod reaches
// it at a bare service IP whose range differs per cluster. That one is
// handled where it belongs, on the client (see kubernetes.NewInCluster).
const inClusterNoProxy = "localhost,127.0.0.1,.svc,.svc.cluster.local,.cluster.local"

// proxyEnv is the forward proxy in the spelling Go's HTTP client, curl, git
// and mise all already read — donkey configures no tool for it individually.
//
// registryHost is donkey's own registry, excluded by name: it is routinely a
// bare service name or an address inside the cluster that no domain suffix
// covers, and sending pushes to a corporate proxy would break every build.
func proxyEnv(p config.InstallProxy, registryHost string) []corev1.EnvVar {
	var env []corev1.EnvVar
	if p.HTTP != "" || p.HTTPS != "" {
		exclude := inClusterNoProxy
		if registryHost != "" {
			exclude += "," + registryHost
		}
		p.NoProxy = strings.Trim(strings.TrimSpace(p.NoProxy)+","+exclude, ",")
	}
	// Both spellings: some tools read only the lowercase form, and HTTP_PROXY
	// in particular is ignored by Go's own client for CGI-safety reasons.
	// A proxy address can carry its own credentials, so the two that can
	// come out of the Secret do. NO_PROXY is a host list and stays plain.
	secret := func(name, key, value string) {
		if value == "" {
			return
		}
		env = append(env, secretEnvVar(name, key), secretEnvVar(strings.ToLower(name), key))
	}
	secret("HTTP_PROXY", "http-proxy", p.HTTP)
	secret("HTTPS_PROXY", "https-proxy", p.HTTPS)
	if p.NoProxy != "" {
		env = append(env,
			corev1.EnvVar{Name: "NO_PROXY", Value: p.NoProxy},
			corev1.EnvVar{Name: "no_proxy", Value: p.NoProxy},
		)
	}
	return env
}

// imagePullPolicy pulls again only when the reference can point somewhere
// else tomorrow. A version tag or a digest is a promise; ":latest" and a bare
// name are not, and a dev install that reuses one would otherwise keep running
// the image the node happens to have cached.
func imagePullPolicy(image string) corev1.PullPolicy {
	if strings.Contains(image, "@") {
		return corev1.PullIfNotPresent
	}
	// Only the last path element can carry the tag — a registry's port colon
	// (reg.example.com:5000/donkey) is not one.
	name := image
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	// "latest" and "dev" are the tags everybody moves; a pod that reused a
	// cached layer for those would run code nobody pushed — and a dev loop
	// (or an e2e run) would silently test a stale server.
	_, tag, ok := strings.Cut(name, ":")
	if !ok || tag == "" || tag == "latest" || tag == "dev" {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}

// serverResources sizes donkey-server. Ephemeral storage is the one nobody
// expects: the server unpacks every build context on local disk (512 MB
// uploaded, up to 2 GB unpacked), and a pod that requests none is the first
// thing the kubelet evicts when a node runs short of it. CPU is requested but
// not capped — throttling the API mid-deploy costs more than the noisy
// neighbour it would prevent.
func serverResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("100m"),
			corev1.ResourceMemory:           resource.MustParse("256Mi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("3Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory:           resource.MustParse("1Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("8Gi"),
		},
	}
}

// buildkitResources sizes the build engine, which needs more of everything: a
// build is the heaviest thing the cluster does, and its cache volume counts
// against the pod's ephemeral storage. The storage limit stays above what the
// cache volume can hold, so the engine's own collector — not an eviction —
// is what keeps the cache in check.
func buildkitResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("500m"),
			corev1.ResourceMemory:           resource.MustParse("1Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("4Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory:           resource.MustParse("8Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("12Gi"),
		},
	}
}

// registryCAFile is where the server reads the registry's certificate
// authority ("" when the registry's certificate is publicly trusted).
func registryCAFile(in SystemInput) string {
	if in.RegistryCASecret == "" {
		return ""
	}
	return registryCAPath + "/ca.crt"
}

// buildkitPodSpec runs the build engine, with the registry's certificate
// authority mounted when the installation named one.
func buildkitPodSpec(in SystemInput, privileged bool) corev1.PodSpec {
	container := corev1.Container{
		Name:  "buildkitd",
		Image: in.BuildkitImage,
		Args:  []string{"--addr", fmt.Sprintf("tcp://0.0.0.0:%d", buildkitPort), "--config", buildkitConfigPath + "/buildkitd.toml"},
		// The proxy is what the engine pulls images through where there is no
		// route to a registry otherwise. Build steps get their own copy from
		// the plan's secrets, not from here.
		Env:             proxyEnv(in.Build.Proxy, registryHostOf(in.RegistryURL)),
		Ports:           []corev1.ContainerPort{{ContainerPort: buildkitPort}},
		SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
		Resources:       buildkitResources(),
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(buildkitPort)},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "buildkit-config", MountPath: buildkitConfigPath, ReadOnly: true},
			{Name: "buildkit-cache", MountPath: buildkitCachePath},
		},
	}
	spec := corev1.PodSpec{
		Containers:        []corev1.Container{container},
		PriorityClassName: systemPriorityClass,
		Volumes: []corev1.Volume{
			{Name: "buildkit-config", VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: buildkitName},
				},
			}},
			// The cache gets a volume of its own so its size is something the
			// cluster enforces, not something a runaway build discovers. It
			// also keeps the snapshotter off the container's own overlay
			// filesystem, where it would fall back to its slow copy mode.
			{Name: "buildkit-cache", VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: new(resource.MustParse(buildkitCacheSize))},
			}},
		},
	}
	if in.RegistryCASecret == "" {
		return spec
	}
	spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{Name: "registry-ca", MountPath: registryCAPath, ReadOnly: true})
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: "registry-ca", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: in.RegistryCASecret},
		},
	})
	return spec
}

// buildkitNetworkPolicy is the one policy donkey cannot do without: the build
// engine speaks unauthenticated gRPC and runs privileged, so anything that
// can reach port 1234 can run code on a node. Only donkey-server may.
func buildkitNetworkPolicy(in SystemInput) *networkingv1.NetworkPolicy {
	port := intstr.FromInt32(buildkitPort)
	tcp := corev1.ProtocolTCP
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      buildkitName,
			Namespace: in.Namespace,
			Labels:    systemLabels(buildkitName),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: systemLabels(buildkitName)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					PodSelector:       &metav1.LabelSelector{MatchLabels: systemLabels(ServerName)},
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": in.Namespace}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
			}},
		},
	}
}

func systemService(in SystemInput, name string, port, targetPort int32) *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: in.Namespace, Labels: systemLabels(name)},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: systemLabels(name),
			Ports:    []corev1.ServicePort{{Name: "http", Port: port, TargetPort: intstr.FromInt32(targetPort)}},
		},
	}
}

// adminsJSON is the console's access rules as the gateway and server read
// them; empty rules stay an empty string (everyone the IdP admits).
func adminsJSON(in SystemInput) string {
	if in.Admins.Empty() {
		return ""
	}
	b, err := json.Marshal(in.Admins)
	if err != nil {
		return ""
	}
	return string(b)
}

// consoleRoute serves the console and API — through the donkey gateway,
// exactly like an app: the gateway runs the browser login (or verifies a
// sealed CLI token) and forwards identity headers; Admins narrows who gets
// in. It is one more entry in the gateway's routing table.
func consoleRoute(in SystemInput) *corev1.ConfigMap {
	ann := map[string]string{
		AnnotationBackend: fmt.Sprintf("http://%s.%s.svc.cluster.local:80", ServerName, in.Namespace),
		AnnotationHost:    in.ServerHost(),
		AnnotationPath:    "/",
		AnnotationLogin:   LoginRequired,
	}
	if a := adminsJSON(in); a != "" {
		ann[AnnotationAccess] = a
	}
	return routeConfigMap(ServerName, in.Namespace, systemLabels(ServerName), ann)
}
