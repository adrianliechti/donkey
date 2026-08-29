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
)

// buildkitConfigHash covers what buildkitd reads at startup: the config file
// itself, which it reads once and never again.
func (in SystemInput) buildkitConfigHash() string {
	// The proxy is secret-backed env, which a pod also only reads at
	// creation — so it belongs in the same hash as the config file.
	return configHash(buildkitTOML(in), in.Build.Proxy.HTTP, in.Build.Proxy.HTTPS, in.Build.Proxy.NoProxy)
}

const (
	// buildkitConfigPath is where buildkitd finds its config file.
	buildkitConfigPath = "/etc/buildkit"

	// buildkitCachePath is buildkitd's state directory — the build cache. It
	// gets a volume of its own, bounded by buildkitCacheSize.
	buildkitCachePath = "/var/lib/buildkit"
	buildkitCacheSize = "10Gi"
	// Both stay below the volume size: collection is asynchronous, so the
	// cache must overshoot without the kubelet evicting the pod holding it.
	buildkitCacheKeep = "5GB"
	buildkitCacheMax  = "8GB"
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
// cache bound alone: without garbage collection the cache grows until the
// node's disk is full. Private-CA registries and mirrors are named here too.
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
		// A bare hostname, never a URL: BuildKit looks it up by host, so a URL
		// key matches nothing and every pull quietly goes to the real
		// registry — the one thing a mirrored installation cannot reach.
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

// mirrorHost yields what BuildKit's `mirrors` list wants: a host with an
// optional path prefix, never a URL. A scheme is accepted (JFrog's UI hands
// out URLs) and answers a different question — whether to use plain http.
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
	// Declared here and not only on the export: the engine also *pulls* from
	// donkey's own registry, and a pull carries no per-solve attribute.
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
		ObjectMeta: metav1.ObjectMeta{Name: buildkitName, Namespace: in.Namespace, Labels: SystemLabels(buildkitName)},
		Data:       map[string]string{"buildkitd.toml": buildkitTOML(in)},
	}
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
				ObjectMeta: in.hashedTemplateMeta(buildkitName, in.buildkitConfigHash()),
				Spec:       buildkitPodSpec(in, privileged),
			},
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
