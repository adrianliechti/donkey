package manifests

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// SystemInput describes the donkey control plane (namespace donkey-system):
// the API server, the in-cluster BuildKit, the shared Gateway (Gateway API,
// Envoy Gateway by default) and the donkey gateway — donkey's own edge
// proxy, which routes every app and runs the OIDC login where a route
// demands one.
type SystemInput struct {
	Namespace   string // "donkey-system"
	ServerImage string
	BaseDomain  string

	// TLSIssuer/TLSIssuerKind name the cert-manager issuer that signs app
	// certificates. TLSCertificateRefs carries the app certificates the
	// gateway already serves, so re-installing never drops them.
	TLSIssuer          string
	TLSIssuerKind      string
	TLSCertificateRefs []string

	// GatewayServiceType is the type of the service traffic enters the
	// cluster through: LoadBalancer (the default) or ClusterIP. Local
	// clusters without a load-balancer controller (kind) need ClusterIP —
	// a LoadBalancer service never gets an address there, which leaves the
	// Gateway unprogrammed and every donkey hostname unroutable.
	GatewayServiceType string

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

	APIToken string

	OIDCIssuer       string // default: http(s)://id.<baseDomain>
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

	BuildkitImage string

	NamespacePrefix string
}

const (
	SystemNamespace = "donkey-system"
	serverName      = "donkey-server"
	buildkitName    = "donkey-buildkitd"
	serverPort      = 8088
	buildkitPort    = 1234

	// GatewayName is the shared Gateway all app routes attach to; the
	// GatewayClass is bound to Envoy Gateway's controller.
	GatewayName           = "donkey"
	GatewayClassName      = "donkey"
	gatewayControllerName = "gateway.envoyproxy.io/gatewayclass-controller"

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

// ServerHost is where the donkey API lives.
func (in SystemInput) ServerHost() string { return "donkey." + in.BaseDomain }

// SystemObjects renders the whole control plane, in apply order.
func SystemObjects(in SystemInput) []runtime.Object {
	objs := []runtime.Object{
		systemNamespace(in),
		envoyProxy(in),
		gatewayClass(in),
		sharedGateway(in),
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
		systemService(in, serverName, 80, serverPort),
		systemRoute(in, serverName, in.ServerHost(), 80),
		redirectRoute(in),
		systemCertificate(in),
	)
	// The donkey gateway is the app edge — always there, login or not.
	return append(objs, donkeyGatewayObjects(in)...)
}

// configHash changes whenever the given configuration values change, so pods
// that read them only at startup get rolled on re-install. Each deployment
// hashes just what it consumes: rotating the API token must not kill in-flight
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
	return configHash(in.APIToken, in.OIDCClientID, in.OIDCClientSecret, in.RegistryPassword, in.OpenAIAPIKey)
}

// buildkitConfigHash covers what buildkitd reads at startup: the config file
// itself, which it reads once and never again.
func (in SystemInput) buildkitConfigHash() string {
	return configHash(buildkitTOML(in))
}

func (in SystemInput) hashedTemplateMeta(name, hash string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Labels:      systemLabels(name),
		Annotations: map[string]string{"donkey.dev/config-hash": hash},
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
		ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: in.Namespace, Labels: systemLabels(serverName)},
	}
}

// serverClusterRoleBinding grants cluster-admin for the PoC: donkey-server
// creates namespaces and everything inside them. A scoped ClusterRole is the
// production follow-up.
func serverClusterRoleBinding(in SystemInput) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: serverName, Labels: systemLabels(serverName)},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: serverName, Namespace: in.Namespace}},
	}
}

func systemSecret(in SystemInput) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: "donkey-system-config", Namespace: in.Namespace, Labels: systemLabels(serverName)},
		StringData: map[string]string{
			"api-token":          in.APIToken,
			"registry-password":  in.RegistryPassword,
			"openai-api-key":     in.OpenAIAPIKey,
			"oidc-client-id":     in.OIDCClientID,
			"oidc-client-secret": in.OIDCClientSecret,
			"session-key":        in.SessionKey,
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

// buildkitTOML is buildkitd's config file. Every installation gets one, for
// the cache bound alone: with no garbage collection the build cache grows
// until the node's disk is full. A registry whose certificate is signed by a
// private CA is named here too — otherwise every push would fail verification.
func buildkitTOML(in SystemInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[worker.oci]\n  gc = true\n  reservedSpace = %q\n  maxUsedSpace = %q\n",
		buildkitCacheKeep, buildkitCacheMax)
	if in.RegistryCASecret != "" {
		host, _, _ := strings.Cut(in.RegistryURL, "/")
		fmt.Fprintf(&b, "\n[registry.%q]\n  ca = [%q]\n", host, registryCAPath+"/ca.crt")
	}
	return b.String()
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
	labels := systemLabels(serverName)
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
		{Name: "DONKEY_BASE_DOMAIN", Value: in.BaseDomain},
		{Name: "DONKEY_TLS_ISSUER", Value: in.TLSIssuer},
		{Name: "DONKEY_TLS_ISSUER_KIND", Value: in.TLSIssuerKind},
		{Name: "DONKEY_BUILDKIT_ADDR", Value: fmt.Sprintf("tcp://%s.%s.svc.cluster.local:%d", buildkitName, in.Namespace, buildkitPort)},
		{Name: "DONKEY_NAMESPACE_PREFIX", Value: in.NamespacePrefix},
		{Name: "DONKEY_DEFAULT_PLATFORM", Value: in.DefaultPlatform},
		{Name: "DONKEY_GATEWAY_NAME", Value: GatewayName},
		{Name: "DONKEY_GATEWAY_NAMESPACE", Value: in.Namespace},
		{Name: "DONKEY_OIDC_ISSUER", Value: in.OIDCIssuer},
		secretEnv("DONKEY_OIDC_CLIENT_ID", "oidc-client-id"),
		secretEnv("DONKEY_OIDC_CLIENT_SECRET", "oidc-client-secret"),
		{Name: "DONKEY_AI_MODEL", Value: in.AIModel},
		{Name: "OPENAI_BASE_URL", Value: in.OpenAIBaseURL},
		secretEnv("DONKEY_API_TOKEN", "api-token"),
		secretEnv("DONKEY_REGISTRY_PASSWORD", "registry-password"),
		secretEnv("OPENAI_API_KEY", "openai-api-key"),
	}
	health := corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(serverPort)},
	}
	spec := corev1.PodSpec{
		ServiceAccountName: serverName,
		PriorityClassName:  systemPriorityClass,
		Containers: []corev1.Container{{
			Name:            "server",
			ImagePullPolicy: imagePullPolicy(in.ServerImage),
			Image:           in.ServerImage,
			Env:             env,
			Ports:           []corev1.ContainerPort{{ContainerPort: serverPort}},
			Resources:       serverResources(),
			// donkey-server holds cluster-admin, so a foothold inside it is
			// the worst one donkey has to offer: it runs as an ordinary user
			// and can gain nothing it wasn't started with.
			SecurityContext: &corev1.SecurityContext{
				RunAsNonRoot:             new(true),
				AllowPrivilegeEscalation: new(false),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
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
		ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: in.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: in.hashedTemplateMeta(serverName, in.serverConfigHash()),
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
		Name:            "buildkitd",
		Image:           in.BuildkitImage,
		Args:            []string{"--addr", fmt.Sprintf("tcp://0.0.0.0:%d", buildkitPort), "--config", buildkitConfigPath + "/buildkitd.toml"},
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
					PodSelector:       &metav1.LabelSelector{MatchLabels: systemLabels(serverName)},
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

func systemRoute(in SystemInput, name, host string, port int32) *gatewayv1.HTTPRoute {
	gwNamespace := gatewayv1.Namespace(in.Namespace)
	section := gatewayv1.SectionName(HTTPSListenerName)
	portNum := gatewayv1.PortNumber(port)
	// Deploys stream build progress for minutes — Envoy's default request
	// timeout would cut the connection.
	noTimeout := gatewayv1.Duration("0s")
	return &gatewayv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: in.Namespace,
			Labels:    systemLabels(name),
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{
					Name:        gatewayv1.ObjectName(GatewayName),
					Namespace:   &gwNamespace,
					SectionName: &section,
				}},
			},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(host)},
			Rules: []gatewayv1.HTTPRouteRule{{
				Timeouts: &gatewayv1.HTTPRouteTimeouts{Request: &noTimeout},
				BackendRefs: []gatewayv1.HTTPBackendRef{{
					BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: gatewayv1.ObjectName(name),
							Port: &portNum,
						},
					},
				}},
			}},
		},
	}
}

// redirectRoute sends every plain-http visitor to https. One catch-all route
// on the http listener covers all apps and the API; cert-manager's HTTP-01
// solver routes carry specific hostnames and paths, which take precedence.
func redirectRoute(in SystemInput) *gatewayv1.HTTPRoute {
	gwNamespace := gatewayv1.Namespace(in.Namespace)
	section := gatewayv1.SectionName("http")
	scheme := "https"
	return &gatewayv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "donkey-https-redirect",
			Namespace: in.Namespace,
			Labels:    systemLabels("donkey-https-redirect"),
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{
					Name:        gatewayv1.ObjectName(GatewayName),
					Namespace:   &gwNamespace,
					SectionName: &section,
				}},
			},
			Rules: []gatewayv1.HTTPRouteRule{{
				Filters: []gatewayv1.HTTPRouteFilter{{
					Type: gatewayv1.HTTPRouteFilterRequestRedirect,
					RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
						Scheme:     &scheme,
						StatusCode: new(301),
					},
				}},
			}},
		},
	}
}

// envoyProxy configures the data plane Envoy Gateway runs for donkey's
// Gateway — above all the type of the service traffic enters through.
func envoyProxy(in SystemInput) *unstructured.Unstructured {
	serviceType := in.GatewayServiceType
	if serviceType == "" {
		serviceType = "LoadBalancer"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.envoyproxy.io/v1alpha1",
		"kind":       "EnvoyProxy",
		"metadata": map[string]any{
			"name":      GatewayClassName,
			"namespace": in.Namespace,
			"labels":    toAnyMap(systemLabels(GatewayClassName)),
		},
		"spec": map[string]any{
			"provider": map[string]any{
				"type": "Kubernetes",
				"kubernetes": map[string]any{
					"envoyService": map[string]any{
						"type": serviceType,
					},
				},
			},
		},
	}}
}

// gatewayClass binds donkey's Gateway to Envoy Gateway (which must already
// be installed in the cluster).
func gatewayClass(in SystemInput) *gatewayv1.GatewayClass {
	ns := gatewayv1.Namespace(in.Namespace)
	return &gatewayv1.GatewayClass{
		TypeMeta: metav1.TypeMeta{APIVersion: "gateway.networking.k8s.io/v1", Kind: "GatewayClass"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   GatewayClassName,
			Labels: systemLabels(GatewayClassName),
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayControllerName,
			ParametersRef: &gatewayv1.ParametersReference{
				Group:     "gateway.envoyproxy.io",
				Kind:      "EnvoyProxy",
				Name:      GatewayClassName,
				Namespace: &ns,
			},
		},
	}
}

// ServerCertName is the certificate for the API's own hostname — donkey
// serves itself the same way it serves apps.
const ServerCertName = "donkey-tls-server"

// systemCertificate asks cert-manager for the API's certificate.
func systemCertificate(in SystemInput) *unstructured.Unstructured {
	kind := in.TLSIssuerKind
	if kind == "" {
		kind = "ClusterIssuer"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]any{
			"name":      ServerCertName,
			"namespace": in.Namespace,
			"labels":    toAnyMap(systemLabels(serverName)),
		},
		"spec": map[string]any{
			"secretName": ServerCertName,
			"dnsNames":   []any{in.ServerHost()},
			"issuerRef": map[string]any{
				"name":  in.TLSIssuer,
				"kind":  kind,
				"group": "cert-manager.io",
			},
		},
	}}
}

// HTTPSListenerName is the listener whose certificate list donkey extends
// with one entry per app (Envoy picks the right one by SNI).
const HTTPSListenerName = "https"

// sharedGateway is the single Gateway every app and system route attaches to;
// routes are allowed from all namespaces. It listens on :80 — plain http and
// where cert-manager's HTTP-01 challenges land — and terminates TLS on :443
// with the app certificates cert-manager issued.
func sharedGateway(in SystemInput) *gatewayv1.Gateway {
	fromAll := gatewayv1.NamespacesFromAll
	allowAll := &gatewayv1.AllowedRoutes{
		Namespaces: &gatewayv1.RouteNamespaces{From: &fromAll},
	}
	mode := gatewayv1.TLSModeTerminate
	// The API's own certificate is always there; the apps' certificates are
	// whatever the gateway already serves.
	certRefs := []gatewayv1.SecretObjectReference{{Name: ServerCertName}}
	for _, name := range in.TLSCertificateRefs {
		if name == ServerCertName {
			continue
		}
		certRefs = append(certRefs, gatewayv1.SecretObjectReference{Name: gatewayv1.ObjectName(name)})
	}
	listeners := []gatewayv1.Listener{
		{
			Name:          "http",
			Port:          80,
			Protocol:      gatewayv1.HTTPProtocolType,
			AllowedRoutes: allowAll,
		},
		{
			Name:     HTTPSListenerName,
			Port:     443,
			Protocol: gatewayv1.HTTPSProtocolType,
			TLS: &gatewayv1.ListenerTLSConfig{
				Mode:            &mode,
				CertificateRefs: certRefs,
			},
			AllowedRoutes: allowAll,
		},
	}
	return &gatewayv1.Gateway{
		TypeMeta: metav1.TypeMeta{APIVersion: "gateway.networking.k8s.io/v1", Kind: "Gateway"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      GatewayName,
			Namespace: in.Namespace,
			Labels:    systemLabels(GatewayName),
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: GatewayClassName,
			Listeners:        listeners,
		},
	}
}
