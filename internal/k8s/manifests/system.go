package manifests

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// SystemInput describes the donkey control plane (namespace donkey-system):
// the API server, the in-cluster BuildKit, and the shared Gateway (Gateway
// API on Envoy Gateway). Login protection is per-app OIDC via Envoy
// SecurityPolicy — configured here, rendered per app.
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

	// RegistryCASecret names a secret in donkey-system holding ca.crt — the
	// certificate authority the registry's own certificate comes from. Both
	// the server and the build engine trust it.
	RegistryCASecret string

	RegistryURL      string
	RegistryUsername string
	RegistryPassword string
	RegistryInsecure bool

	APIToken string

	OIDCIssuer       string // default: http(s)://id.<baseDomain>
	OIDCClientID     string
	OIDCClientSecret string

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
		gatewayClass(in),
		sharedGateway(in),
		serverServiceAccount(in),
		serverClusterRoleBinding(in),
		systemSecret(in),
	}
	// The build engine only needs its own config file when it has to trust a
	// registry certificate that isn't publicly signed — and it has to exist
	// before the deployment that mounts it.
	if in.RegistryCASecret != "" {
		objs = append(objs, buildkitConfig(in))
	}
	return append(objs,
		buildkitDeployment(in),
		systemService(in, buildkitName, buildkitPort, buildkitPort),
		serverDeployment(in),
		systemService(in, serverName, 80, serverPort),
		systemRoute(in, serverName, in.ServerHost(), 80),
		systemCertificate(in),
	)
}

// configHash changes whenever secret-backed configuration changes, so pods
// whose env comes from the secret get rolled on re-install (secretKeyRef env
// values are only read at pod creation).
func (in SystemInput) configHash() string {
	h := sha256.New()
	for _, v := range []string{
		in.APIToken, in.OIDCClientID, in.OIDCClientSecret,
		in.RegistryPassword, in.OpenAIAPIKey,
	} {
		fmt.Fprintln(h, v)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (in SystemInput) hashedTemplateMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Labels:      systemLabels(name),
		Annotations: map[string]string{"donkey.dev/config-hash": in.configHash()},
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
		},
	}
}

// registryCAPath is where both the server and buildkitd find the registry's
// certificate authority.
const registryCAPath = "/etc/donkey/registry-ca"

// buildkitConfig teaches buildkitd about a registry whose certificate is
// signed by a private CA — otherwise every push would fail verification.
func buildkitConfig(in SystemInput) *corev1.ConfigMap {
	host, _, _ := strings.Cut(in.RegistryURL, "/")
	toml := fmt.Sprintf("[registry.%q]\n  ca = [%q]\n", host, registryCAPath+"/ca.crt")
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: buildkitName, Namespace: in.Namespace, Labels: systemLabels(buildkitName)},
		Data:       map[string]string{"buildkitd.toml": toml},
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
				// The config hash rolls buildkitd when the registry's CA changes.
				ObjectMeta: in.hashedTemplateMeta(buildkitName),
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
		{Name: "DONKEY_REGISTRY_INSECURE", Value: boolStr(in.RegistryInsecure)},
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
	spec := corev1.PodSpec{
		ServiceAccountName: serverName,
		Containers: []corev1.Container{{
			Name: "server",
			// Always pull: dev installs reuse mutable tags.
			ImagePullPolicy: corev1.PullAlways,
			Image:           in.ServerImage,
			Env:             env,
			Ports:           []corev1.ContainerPort{{ContainerPort: serverPort}},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(serverPort)},
				},
			},
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
				ObjectMeta: in.hashedTemplateMeta(serverName),
				Spec:       spec,
			},
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
		Args:            []string{"--addr", fmt.Sprintf("tcp://0.0.0.0:%d", buildkitPort)},
		Ports:           []corev1.ContainerPort{{ContainerPort: buildkitPort}},
		SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(buildkitPort)},
			},
		},
	}
	spec := corev1.PodSpec{Containers: []corev1.Container{container}}
	if in.RegistryCASecret == "" {
		return spec
	}
	spec.Containers[0].Args = append(spec.Containers[0].Args, "--config", "/etc/buildkit/buildkitd.toml")
	spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
		{Name: "registry-ca", MountPath: registryCAPath, ReadOnly: true},
		{Name: "buildkit-config", MountPath: "/etc/buildkit", ReadOnly: true},
	}
	spec.Volumes = []corev1.Volume{
		{Name: "registry-ca", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: in.RegistryCASecret},
		}},
		{Name: "buildkit-config", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: buildkitName},
			},
		}},
	}
	return spec
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
					Name:      gatewayv1.ObjectName(GatewayName),
					Namespace: &gwNamespace,
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

// gatewayClass binds donkey's Gateway to Envoy Gateway (which must already
// be installed in the cluster).
func gatewayClass(in SystemInput) *gatewayv1.GatewayClass {
	return &gatewayv1.GatewayClass{
		TypeMeta: metav1.TypeMeta{APIVersion: "gateway.networking.k8s.io/v1", Kind: "GatewayClass"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   GatewayClassName,
			Labels: systemLabels(GatewayClassName),
		},
		Spec: gatewayv1.GatewayClassSpec{ControllerName: gatewayControllerName},
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

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
