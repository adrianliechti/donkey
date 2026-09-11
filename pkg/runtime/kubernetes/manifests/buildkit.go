package manifests

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/adrianliechti/donkey/internal/config"
)

// buildkitConfigHash covers what buildkitd reads from its startup pod: the
// config file, proxy environment, and mounted CA bundle generation.
func (in SystemInput) buildkitConfigHash() string {
	// The proxy is secret-backed env, which a pod also only reads at
	// creation — so it belongs in the same hash as the config file.
	return configHash(buildkitTOML(in), in.Build.Proxy.HTTP, in.Build.Proxy.HTTPS, in.Build.Proxy.NoProxy, in.RegistryCAFingerprint)
}

const (
	// buildkitConfigPath is where buildkitd finds its config file.
	buildkitConfigPath = "/etc/buildkit"

	// buildkitCachePath holds reusable layers. BuildKit manages cache space
	// using its own disk-aware garbage collection policy.
	buildkitCachePath = "/var/lib/buildkit"
)

// BuildkitConfig is the build engine's config file. Both runtimes render it
// from here, so a mirrored installation behaves the same on either.
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

// registrySection is one [registry."host"] table. Hosts are collected first
// because donkey's registry and a mirror can be the same JFrog, and TOML
// rejects a table declared twice.
type registrySection struct {
	mirrors   []string
	caFile    string
	plainHTTP bool
}

// BuildkitTOML renders the config file. Every installation gets one for the
// garbage collector; no fixed disk-size ceiling is imposed here. Private-CA registries and mirrors are named here too.
func BuildkitTOML(c BuildkitConfig) string {
	var b strings.Builder
	b.WriteString("[worker.oci]\n  gc = true\n")

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
		s := at(strings.ToLower(c.RegistryHost))
		s.caFile = c.RegistryCAFile
		s.plainHTTP = c.RegistryPlainHTTP
	}
	for upstream, mirror := range c.Mirrors {
		target, targetErr := config.ParseRegistryMirror(mirror)
		source, sourceErr := config.NormalizeRegistrySource(upstream)
		if sourceErr != nil || targetErr != nil {
			continue
		}
		up := at(source)
		up.mirrors = append(up.mirrors, target.Address)
		// The mirror is a registry in its own right: donkey reaches it over
		// the scheme it was given, and an in-house one usually sits behind
		// the same private CA as donkey's registry.
		m := at(target.Host)
		m.plainHTTP = m.plainHTTP || target.PlainHTTP
		if c.RegistryCAFile != "" && !target.PlainHTTP {
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

// registryHostOf is the host part of donkey's registry setting, which is a
// prefix ("registry.example.com/donkey") rather than a bare host.
func registryHostOf(registryURL string) string {
	host, _, _ := strings.Cut(registryURL, "/")
	return strings.ToLower(host)
}

func buildkitTOML(in SystemInput) string {
	// Declared here and not only on the export: the engine also *pulls* from
	// donkey's own registry, and a pull carries no per-solve attribute.
	c := BuildkitConfig{
		RegistryHost:      registryHostOf(in.RegistryURL),
		RegistryPlainHTTP: in.RegistryInsecure,
		Mirrors:           in.Build.RegistryMirrors,
	}
	if in.RegistryCASecret != "" {
		c.RegistryCAFile = registryCAPath + "/ca.crt"
	}
	return BuildkitTOML(c)
}

func buildkitDeployment(in SystemInput) *appsv1.Deployment {
	labels := SystemLabels(buildkitName)
	replicas := int32(1)
	privileged := true
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: buildkitName, Namespace: in.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: SystemSelector(buildkitName)},
			Template: corev1.PodTemplateSpec{
				// The config hash rolls buildkitd when the registry setup changes.
				ObjectMeta: in.hashedTemplateMeta(buildkitName, in.BuildkitImage, in.buildkitConfigHash()),
				Spec:       buildkitPodSpec(in, privileged),
			},
		},
	}
}

// buildkitResources sizes the build engine, which needs more of everything: a
// build is the heaviest thing the cluster does. Requests reserve capacity;
// the engine's garbage collector manages cached layers without pod limits.
func buildkitResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("500m"),
			corev1.ResourceMemory:           resource.MustParse("1Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("4Gi"),
		},
	}
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
		ImagePullSecrets:  systemImagePullSecrets(in),
		Volumes: []corev1.Volume{
			{Name: "buildkit-config", VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: SystemConfigMapName},
					Items:                []corev1.KeyToPath{{Key: "buildkitd.toml", Path: "buildkitd.toml"}},
				},
			}},
			// The cache gets a volume of its own so its size is something the
			// cluster enforces, not something a runaway build discovers. It
			// also keeps the snapshotter off the container's own overlay
			// filesystem, where it would fall back to its slow copy mode.
			{Name: "buildkit-cache", VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
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
