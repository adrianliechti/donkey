package manifests

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	// or "none" (no TLS at donkey's edge — behind a TLS-terminating cluster
	// gateway, whose certificates are the platform's prerequisite).
	TLSMode string
	// ACMEEmail is the ACME account's contact (expiry warnings); optional.
	ACMEEmail string
	// ACMEDirectory overrides the ACME endpoint (default: Let's Encrypt
	// production) — staging, or an internal ACME CA.
	ACMEDirectory string
	// HTTPRouteGateway, when named, attaches the donkey gateway to a Gateway
	// API gateway the cluster already runs instead of relying on the edge
	// Service being reachable — the arrangement for clusters whose traffic
	// must enter through a standard edge. Pairs with TLS mode "none": the
	// cluster gateway terminates TLS before an HTTPRoute matches.
	HTTPRouteGateway GatewayRef
	// AppGateway asks donkey-server to create one namespaced Gateway,
	// HTTPRoute and Certificate for every externally reachable app.
	AppGateway config.InstallAppGateway
	// ContourHTTPProxy asks donkey-server to create a Certificate and root
	// HTTPProxy per app. SystemObjects also creates the equivalent edge for
	// the console and central OAuth callback.
	ContourHTTPProxy config.InstallContourHTTPProxy

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
	// certificate authority the enterprise registry and package upstreams
	// come from. The server, build engine and package proxy trust it.
	RegistryCASecret string
	// RegistryDockerConfigSecret contains per-host auths entries for
	// separately hosted BuildKit mirrors and is read dynamically by the
	// server's BuildKit session auth provider.
	RegistryDockerConfigSecret string

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
	// TelemetryKey authenticates OTLP ingestion and private queries. It is
	// generated and persisted independently so session rotation cannot break
	// exporters embedded in already-running apps.
	TelemetryKey string

	OpenAIBaseURL string
	OpenAIAPIKey  string
	AIModel       string

	// DefaultPlatform is the build platform of the cluster's nodes.
	DefaultPlatform string

	// Build is the builder's settings, the mirrors every build pulls through,
	// and the proxy donkey's own processes use.
	Build config.InstallBuild
	// AddonImages are pulled by the runtime, outside BuildKit.
	AddonImages config.InstallAddonImages
	// AddonImagePullSecrets are existing credentials in every workload
	// namespace, referenced by managed service pods without copying them.
	AddonImagePullSecrets []string

	BuildkitImage string

	// NamespacePrefix maps a space onto its own namespace; ignored when
	// WorkloadNamespace names one namespace for the whole installation.
	NamespacePrefix string
	// WorkloadNamespace, when set, is the single namespace every app lives
	// in. It belongs to whoever created it: donkey neither creates nor
	// deletes it, and needs no cluster-scoped rights at all — see
	// serverAccessObjects.
	WorkloadNamespace string
	// SkipNamespace leaves the control plane's own namespace out of the
	// render, because it already exists. donkey never adopts a namespace it
	// didn't create: applying one would take ownership of its labels and mark
	// it deletable.
	SkipNamespace bool
	// IsolateSpaces fences every space with a NetworkPolicy — see
	// SpaceIsolationPolicy. On by default; the installer resolves the
	// settings file's tri-state (unset means on) before it reaches here.
	// Needs a CNI that enforces NetworkPolicy.
	IsolateSpaces bool
	// RestrictEgress adds the egress half of that fence — a space's pods reach
	// only their own space, donkey-ai, donkey-telemetry and DNS, never the
	// internet. Off by default; only meaningful with IsolateSpaces.
	RestrictEgress bool
}

const (
	// DefaultSystemNamespace is where the control plane lives unless an
	// installation says otherwise.
	DefaultSystemNamespace = config.DefaultSystemNamespace
	// ServerName is donkey-server's Service — the openai addon points app
	// bindings at its AI proxy path.
	ServerName = "donkey-server"
	// SystemConfigSecretName holds the installation credentials and the
	// settings intentionally kept out of pod specs.
	SystemConfigSecretName = "donkey-system-config"
	// SystemRegistrySecretName lets Kubernetes pull credentialed control-plane
	// images. It is exported for offline deployment bundle renderers.
	SystemRegistrySecretName = "donkey-system-registry"
	buildkitName             = "donkey-buildkitd"
	registryDockerConfigPath = "/etc/donkey/registry-auth"
	serverPort               = 8088
	buildkitPort             = 1234

	// The image names no user (hack/Dockerfile.server) because only the
	// runtime can enforce one. Everything the server writes goes to /tmp, so
	// any uid works.
	systemRunAsUser = int64(1000)

	// Keeps the control plane ahead of the apps it runs — donkey-server going
	// down takes every deploy and log stream with it. Shipped in every
	// cluster, so donkey installs no PriorityClass of its own.
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

// Shared reports whether the whole installation shares one app namespace.
func (in SystemInput) Shared() bool { return in.WorkloadNamespace != "" }

// SystemObjects renders the whole control plane, in apply order.
func SystemObjects(in SystemInput) []runtime.Object {
	var objs []runtime.Object
	if !in.SkipNamespace {
		objs = append(objs, systemNamespace(in))
	}
	objs = append(objs,
		serverServiceAccount(in),
	)
	objs = append(objs, serverAccessObjects(in)...)
	objs = append(objs, systemSecret(in))
	if hasSystemRegistryCredentials(in) {
		objs = append(objs, systemRegistryPullSecret(in))
	}
	// The build engine's config file has to exist before the deployment that
	// mounts it.
	objs = append(objs, buildkitConfig(in))
	// Package traffic has its own unprivileged process and credentials. It is
	// ready before builds are accepted by donkey-server.
	objs = append(objs, packagesObjects(in)...)
	// One replica owns the embedded DuckDB file and exposes OTLP/HTTP plus a
	// separate control-plane query port.
	objs = append(objs, telemetryObjects(in)...)
	objs = append(objs,
		buildkitDeployment(in),
		systemService(in, buildkitName, buildkitPort, buildkitPort),
		buildkitNetworkPolicy(in),
		serverNetworkPolicy(in),
		serverDeployment(in),
		systemService(in, ServerName, 80, serverPort),
		consoleRoute(in),
	)
	// The donkey gateway is the app edge — always there, login or not; the
	// AI endpoint is the one workload apps reach, and runs apart from the
	// admin API for exactly that reason.
	objs = append(objs, donkeyGatewayObjects(in)...)
	// Contour's shared-Gateway mode puts downstream TLS on root HTTPProxies.
	// Include the control plane and callback here; app HTTPProxies arrive with
	// their deploys through the same shared data plane.
	objs = append(objs, contourSystemEdgeObjects(in)...)
	objs = append(objs, aiObjects(in)...)
	// The one optional attachment to machinery the cluster already runs —
	// rendered only when named, so a plain installation stays CRD-free.
	if in.HTTPRouteGateway.Attached() {
		objs = append(objs, httpRoute(in))
	}
	return objs
}

// configHash rolls pods that read config only at startup. Each deployment
// hashes just what it consumes: rotating the session key must not kill
// in-flight builds, and a new registry host must roll buildkitd.
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
	return configHash(in.SessionKey, in.TelemetryKey, in.OIDCClientID, in.OIDCClientSecret, in.RegistryPassword, in.OpenAIAPIKey,
		buildEnvJSON(in.Build), in.Build.Instructions, in.Build.Proxy.HTTP, in.Build.Proxy.HTTPS)
}

// hashedTemplateMeta is the pod template metadata of one control-plane
// deployment: the full descriptive label set (version from the image that
// actually runs in the pod) plus the config hash that rolls it.
func (in SystemInput) hashedTemplateMeta(name, image, hash string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Labels:      withVersion(SystemLabels(name), image),
		Annotations: map[string]string{AnnotationConfigHash: hash},
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

// No way to gain more than it started with. donkey-server holds
// cluster-admin, so a foothold inside it is the worst donkey has to offer.
func systemSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             new(true),
		AllowPrivilegeEscalation: new(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func systemNamespace(in SystemInput) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        in.Namespace,
			Labels:      SystemLabels("donkey-system"),
			Annotations: map[string]string{AnnotationManaged: "true"},
		},
	}
}

func serverServiceAccount(in SystemInput) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: ServerName, Namespace: in.Namespace, Labels: SystemLabels(ServerName)},
	}
}

func systemSecret(in SystemInput) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: SystemConfigSecretName, Namespace: in.Namespace, Labels: SystemLabels(ServerName)},
		StringData: map[string]string{
			"registry-password":  in.RegistryPassword,
			"openai-api-key":     in.OpenAIAPIKey,
			"oidc-client-id":     in.OIDCClientID,
			"oidc-client-secret": in.OIDCClientSecret,
			"session-key":        in.SessionKey,
			"telemetry-key":      in.TelemetryKey,
			// Build environment and proxy settings routinely carry credentials
			// ("https://ci:token@jfrog.corp"); builder instructions can expose
			// internal registry topology. Keep all three out of the Deployment.
			"build-env":            buildEnvJSON(in.Build),
			"builder-instructions": in.Build.Instructions,
			"http-proxy":           in.Build.Proxy.HTTP,
			"https-proxy":          in.Build.Proxy.HTTPS,
			"packages-password":    in.Build.Packages.Password,
		},
	}
}

func systemRegistryPullSecret(in SystemInput) *corev1.Secret {
	return RegistryPullSecret(
		SystemRegistrySecretName, in.Namespace,
		SystemLabels(RoleRegistryAuth),
		registryHostOf(in.RegistryURL), in.RegistryUsername, in.RegistryPassword,
	)
}

func systemImagePullSecrets(in SystemInput) []corev1.LocalObjectReference {
	if !hasSystemRegistryCredentials(in) {
		return nil
	}
	return []corev1.LocalObjectReference{{Name: SystemRegistrySecretName}}
}

func hasSystemRegistryCredentials(in SystemInput) bool {
	return registryHostOf(in.RegistryURL) != "" && in.RegistryUsername != "" && in.RegistryPassword != ""
}

// registryCAPath is where both the server and buildkitd find the registry's
// certificate authority.
const registryCAPath = "/etc/donkey/registry-ca"

func serverDeployment(in SystemInput) *appsv1.Deployment {
	labels := withVersion(SystemLabels(ServerName), in.ServerImage)
	replicas := int32(1)
	secretEnv := func(name, key string) corev1.EnvVar {
		return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: SystemConfigSecretName},
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
		{Name: "DONKEY_BUILDKIT_ADDR", Value: fmt.Sprintf("tcp://%s.%s.svc:%d", buildkitName, in.Namespace, buildkitPort)},
		{Name: "DONKEY_NAMESPACE_PREFIX", Value: in.NamespacePrefix},
		{Name: "DONKEY_SYSTEM_NAMESPACE", Value: in.Namespace},
		{Name: "DONKEY_WORKLOAD_NAMESPACE", Value: in.WorkloadNamespace},
		{Name: "DONKEY_SPACE_ISOLATION", Value: strconv.FormatBool(in.IsolateSpaces)},
		{Name: "DONKEY_RESTRICT_EGRESS", Value: strconv.FormatBool(in.RestrictEgress)},
		// Where the console's /ai/v1 requests are forwarded. The address
		// only — the path the console asks for is what gets proxied.
		{Name: "DONKEY_AI_URL", Value: fmt.Sprintf("http://%s.%s.svc:80", AIAppName, in.Namespace)},
		{Name: "DONKEY_TELEMETRY_URL", Value: TelemetryAPIEndpoint(in.Namespace)},
		{Name: "DONKEY_TELEMETRY_OTLP_ENDPOINT", Value: TelemetryOTLPEndpoint(in.Namespace)},
		{Name: "DONKEY_DEFAULT_PLATFORM", Value: in.DefaultPlatform},
		{Name: "DONKEY_POSTGRES_IMAGE", Value: in.AddonImages.Postgres},
		{Name: "DONKEY_RUSTFS_IMAGE", Value: in.AddonImages.RustFS},
		{Name: "DONKEY_ADDON_IMAGE_PULL_SECRETS", Value: strings.Join(in.AddonImagePullSecrets, ",")},
		{Name: "DONKEY_APP_GATEWAY_CLASS_NAME", Value: in.AppGateway.ClassName},
		{Name: "DONKEY_APP_GATEWAY_NAME_TEMPLATE", Value: in.AppGateway.NameTemplate},
		{Name: "DONKEY_APP_GATEWAY_ROUTE_NAMESPACE", Value: in.AppGateway.RouteNamespace},
		{Name: "DONKEY_APP_GATEWAY_ISSUER_NAME", Value: in.AppGateway.IssuerName},
		{Name: "DONKEY_APP_GATEWAY_ISSUER_KIND", Value: in.AppGateway.IssuerKind},
		{Name: "DONKEY_APP_GATEWAY_ISSUER_GROUP", Value: in.AppGateway.IssuerGroup},
		{Name: "DONKEY_CONTOUR_HTTP_PROXY_INGRESS_CLASS_NAME", Value: in.ContourHTTPProxy.IngressClassName},
		{Name: "DONKEY_CONTOUR_HTTP_PROXY_ISSUER_NAME", Value: in.ContourHTTPProxy.IssuerName},
		{Name: "DONKEY_CONTOUR_HTTP_PROXY_ISSUER_KIND", Value: in.ContourHTTPProxy.IssuerKind},
		{Name: "DONKEY_CONTOUR_HTTP_PROXY_ISSUER_GROUP", Value: in.ContourHTTPProxy.IssuerGroup},
		{Name: "DONKEY_OIDC_ISSUER", Value: in.OIDCIssuer},
		secretEnv("DONKEY_OIDC_CLIENT_ID", "oidc-client-id"),
		secretEnv("DONKEY_OIDC_CLIENT_SECRET", "oidc-client-secret"),
		{Name: "DONKEY_AI_MODEL", Value: in.AIModel},
		{Name: "OPENAI_BASE_URL", Value: in.OpenAIBaseURL},
		secretEnv("DONKEY_BUILDER_INSTRUCTIONS", "builder-instructions"),
		// The session key lets the server verify sealed CLI tokens itself —
		// the direct (port-forwarded) path next to the gateway's headers.
		secretEnv("DONKEY_SESSION_KEY", "session-key"),
		secretEnv("DONKEY_TELEMETRY_KEY", "telemetry-key"),
		{Name: "DONKEY_ADMINS", Value: adminsJSON(in)},
		secretEnv("DONKEY_REGISTRY_PASSWORD", "registry-password"),
		secretEnv("OPENAI_API_KEY", "openai-api-key"),
	}
	if in.RegistryDockerConfigSecret != "" {
		env = append(env, corev1.EnvVar{Name: "DONKEY_REGISTRY_CONFIG_DIR", Value: registryDockerConfigPath})
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
		ImagePullSecrets:   systemImagePullSecrets(in),
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
		spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: "registry-ca", MountPath: registryCAPath, ReadOnly: true})
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name: "registry-ca",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: in.RegistryCASecret},
			},
		})
	}
	if in.RegistryDockerConfigSecret != "" {
		spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: "registry-auth", MountPath: registryDockerConfigPath, ReadOnly: true})
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name: "registry-auth",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: in.RegistryDockerConfigSecret},
			},
		})
	}
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: ServerName, Namespace: in.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: SystemSelector(ServerName)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: in.hashedTemplateMeta(ServerName, in.ServerImage, in.serverConfigHash()),
				Spec:       spec,
			},
		},
	}
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
	// Everybody moves "latest" and "dev": a pod reusing a cached layer for
	// those runs code nobody pushed, and an e2e run tests a stale server.
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

// registryCAFile is where the server reads the registry's certificate
// authority ("" when the registry's certificate is publicly trusted).
func registryCAFile(in SystemInput) string {
	if in.RegistryCASecret == "" {
		return ""
	}
	return registryCAPath + "/ca.crt"
}

func systemService(in SystemInput, name string, port, targetPort int32) *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: in.Namespace, Labels: SystemLabels(name)},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: SystemSelector(name),
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
		AnnotationBackend: fmt.Sprintf("http://%s.%s.svc:80", ServerName, in.Namespace),
		AnnotationHost:    in.ServerHost(),
		AnnotationPath:    "/",
		AnnotationLogin:   LoginRequired,
		// The server verifies identity itself — it gets the sealed token,
		// not bare headers anything in the cluster could type.
		AnnotationToken: TokenForward,
	}
	if a := adminsJSON(in); a != "" {
		ann[AnnotationAccess] = a
	}
	return routeConfigMap(ServerName, in.Namespace, SystemLabels(ServerName), ann)
}
