package manifests

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	ProxyAppName    = "donkey-proxy"
	proxyPort       = 8091
	ProxyConfigKey  = "services.json"
	proxyConfigPath = "/etc/donkey/proxy/services.json"
)

func proxyObjects(in SystemInput) []runtime.Object {
	objects := []runtime.Object{
		&corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Name: ProxyAppName, Namespace: in.Namespace, Labels: SystemLabels(ProxyAppName)},
			Data:       map[string][]byte{ProxyConfigKey: []byte(in.ProxyServices.JSON())},
		},
		&corev1.ServiceAccount{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
			ObjectMeta: metav1.ObjectMeta{Name: ProxyAppName, Namespace: in.Namespace, Labels: SystemLabels(ProxyAppName)},
		},
	}
	objects = append(objects, bindingTokenAccess(in, ProxyAppName, "pods")...)
	peers := append(appPeers(in), systemPeer(in.Namespace, ServerName))
	return append(objects, proxyDeployment(in), systemService(in, ProxyAppName, 80, proxyPort),
		ingressPolicy(in, ProxyAppName, proxyPort, peers))
}

func proxyDeployment(in SystemInput) *appsv1.Deployment {
	health := corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(proxyPort)}}
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: ProxyAppName, Namespace: in.Namespace, Labels: SystemLabels(ProxyAppName)},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: SystemSelector(ProxyAppName)},
			Template: corev1.PodTemplateSpec{
				// The mounted Secret is reloaded at runtime; changing targets
				// does not restart the shared proxy or its in-flight requests.
				ObjectMeta: in.hashedTemplateMeta(ProxyAppName, in.ServerImage, configHash(in.TelemetryKey)),
				Spec: corev1.PodSpec{
					ServiceAccountName: ProxyAppName, PriorityClassName: systemPriorityClass,
					SecurityContext: systemPodSecurityContext(), ImagePullSecrets: systemImagePullSecrets(in),
					Volumes: []corev1.Volume{{Name: "configuration", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
						SecretName: ProxyAppName, Items: []corev1.KeyToPath{{Key: ProxyConfigKey, Path: ProxyConfigKey}},
					}}}},
					Containers: []corev1.Container{{
						Name: "proxy", Image: in.ServerImage, ImagePullPolicy: imagePullPolicy(in.ServerImage), Args: []string{"proxy"},
						Env: []corev1.EnvVar{
							{Name: "DONKEY_LISTEN", Value: ":8091"},
							{Name: "DONKEY_PROXY_SERVICES_FILE", Value: proxyConfigPath},
							{Name: "DONKEY_SYSTEM_NAMESPACE", Value: in.Namespace},
							{Name: "DONKEY_WORKLOAD_NAMESPACE", Value: in.WorkloadNamespace},
							{Name: "DONKEY_NAMESPACE_PREFIX", Value: in.NamespacePrefix},
							secretEnvVar("DONKEY_TELEMETRY_KEY", "DONKEY_TELEMETRY_KEY"),
						},
						VolumeMounts:    []corev1.VolumeMount{{Name: "configuration", MountPath: "/etc/donkey/proxy", ReadOnly: true}},
						Ports:           []corev1.ContainerPort{{ContainerPort: proxyPort, Name: "http"}},
						Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")}},
						SecurityContext: systemSecurityContext(),
						ReadinessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(proxyPort)}}},
						LivenessProbe:   &corev1.Probe{ProbeHandler: health, PeriodSeconds: 15, FailureThreshold: 3},
					}},
				},
			},
		},
	}
}
