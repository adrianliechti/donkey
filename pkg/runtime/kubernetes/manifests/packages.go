package manifests

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	packagesName             = "donkey-packages"
	packagesPort             = 8090
	packagesDockerConfigPath = "/etc/donkey/package-auth"
)

// packagesObjects is the one egress point package-manager traffic from
// Dockerfile RUN instructions uses. Only BuildKit may connect to it; it has
// no service account or cluster permissions.
func packagesObjects(in SystemInput) []runtime.Object {
	return []runtime.Object{
		packagesDeployment(in),
		systemService(in, packagesName, 80, packagesPort),
		packagesNetworkPolicy(in),
	}
}

func (in SystemInput) packagesConfigHash() string {
	p := in.Build.Packages
	return configHash(
		p.NPMUpstream(), p.PyPIUpstream(), p.GoUpstream(), p.MavenUpstream(), p.NuGetUpstream(),
		strings.Join(p.ResourceHosts, ","), strings.Join(p.CredentialHosts, ","),
		p.Username, p.Password, p.DockerConfigSecret, p.DockerConfigHost,
		in.Build.Proxy.HTTP, in.Build.Proxy.HTTPS, in.Build.Proxy.NoProxy,
		in.RegistryCAFingerprint,
	)
}

func packagesDeployment(in SystemInput) *appsv1.Deployment {
	labels := SystemLabels(packagesName)
	replicas := int32(1)
	p := in.Build.Packages
	env := []corev1.EnvVar{
		{Name: "DONKEY_LISTEN", Value: fmt.Sprintf(":%d", packagesPort)},
		{Name: "DONKEY_PACKAGES_NPM_URL", Value: p.NPMUpstream()},
		{Name: "DONKEY_PACKAGES_PYPI_URL", Value: p.PyPIUpstream()},
		{Name: "DONKEY_PACKAGES_GO_URL", Value: p.GoUpstream()},
		{Name: "DONKEY_PACKAGES_MAVEN_URL", Value: p.MavenUpstream()},
		{Name: "DONKEY_PACKAGES_NUGET_URL", Value: p.NuGetUpstream()},
		{Name: "DONKEY_PACKAGES_RESOURCE_HOSTS", Value: strings.Join(p.ResourceHosts, ",")},
		{Name: "DONKEY_PACKAGES_CREDENTIAL_HOSTS", Value: strings.Join(p.CredentialHosts, ",")},
		{Name: "DONKEY_PACKAGES_USERNAME", Value: p.Username},
	}
	if p.Password != "" {
		env = append(env, secretEnvVar("DONKEY_PACKAGES_PASSWORD", "packages-password"))
	}
	if in.RegistryCASecret != "" {
		env = append(env, corev1.EnvVar{Name: "DONKEY_PACKAGES_CA_FILE", Value: registryCAPath + "/ca.crt"})
	}
	if p.DockerConfigSecret != "" {
		env = append(env,
			corev1.EnvVar{Name: "DONKEY_PACKAGES_DOCKER_CONFIG_DIR", Value: packagesDockerConfigPath},
			corev1.EnvVar{Name: "DONKEY_PACKAGES_DOCKER_CONFIG_HOST", Value: p.DockerConfigHost},
		)
	}
	// The package endpoint itself owns outbound requests, so it needs the
	// installation's forward proxy. Builds connect to it over cluster DNS.
	env = append(env, proxyEnv(in.Build.Proxy, registryHostOf(in.RegistryURL))...)

	health := corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(packagesPort)},
	}
	spec := corev1.PodSpec{
		PriorityClassName:            systemPriorityClass,
		SecurityContext:              systemPodSecurityContext(),
		AutomountServiceAccountToken: new(false),
		ImagePullSecrets:             systemImagePullSecrets(in),
		Containers: []corev1.Container{{
			Name:            "packages",
			Image:           in.ServerImage,
			ImagePullPolicy: imagePullPolicy(in.ServerImage),
			Args:            []string{"packages"},
			Env:             env,
			Ports:           []corev1.ContainerPort{{ContainerPort: packagesPort, Name: "http"}},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("50m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
			},
			SecurityContext: systemSecurityContext(),
			ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(packagesPort)},
			}},
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
	if p.DockerConfigSecret != "" {
		spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: "package-auth", MountPath: packagesDockerConfigPath, ReadOnly: true})
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name: "package-auth",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: p.DockerConfigSecret},
			},
		})
	}
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: packagesName, Namespace: in.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: SystemSelector(packagesName)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: in.hashedTemplateMeta(packagesName, in.ServerImage, in.packagesConfigHash()),
				Spec:       spec,
			},
		},
	}
}

func packagesNetworkPolicy(in SystemInput) *networkPolicy {
	return ingressPolicy(in, packagesName, packagesPort, []networkingv1.NetworkPolicyPeer{
		systemPeer(in.Namespace, buildkitName),
	})
}
