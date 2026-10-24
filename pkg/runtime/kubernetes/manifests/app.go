// Package manifests turns donkey configuration into Kubernetes objects.
// Everything here is pure — no I/O, no cluster — so it is golden-testable,
// and `--dry-run` renders exactly what deploy applies.
package manifests

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/adrianliechti/donkey/internal/config"
)

const (
	// WebContainerName is the app container in the pod. Exported because the
	// runtime reads container status and metrics by this name.
	WebContainerName = "web"

	// storageVolumeName is the app's single volume; every configured path
	// becomes a folder on it.
	storageVolumeName = "data"
	// storageFSGroup owns that volume so any container user can write to it.
	storageFSGroup = int64(2000)
)

// Input is everything needed to render an app's core objects.
type Input struct {
	App *config.AppConfig
	// Space is the app's space; Namespace is the namespace that space maps to.
	Space     string
	Namespace string
	// SharedNamespace marks an installation whose apps all live in one
	// namespace donkey doesn't own: there is no namespace to render, and the
	// space is recorded by a marker object instead.
	SharedNamespace bool
	// IsolateSpace renders the space's network fence next to its home — see
	// SpaceIsolationPolicy. RestrictEgress adds the egress half. SystemNamespace
	// is the control plane's namespace (the installation identity), which names
	// the fence's admitted peers.
	IsolateSpace    bool
	RestrictEgress  bool
	SystemNamespace string
	// ObjectName is what this app's objects are called. It is App.Name unless
	// the installation shares one namespace, where every name carries its
	// space. Empty means the two are the same.
	ObjectName string
	ImageRef   string
	// RegistryHost/Username/Password feed the image pull secret; empty
	// username or password skips it (anonymous registry).
	RegistryHost     string
	RegistryUsername string
	RegistryPassword string
	// AddonSecretNames lists binding secrets injected via envFrom.
	AddonSecretNames []string
	// Domain is the hostname the app is served on; empty skips the route.
	Domain string
	// TLS provisions the app's exact-host Certificate in cert-manager mode.
	// The generated Secret lives with the gateway, which consumes it directly.
	TLS config.InstallTLS
	// ExternalDNS optionally publishes the same hostname at the gateway VIP.
	ExternalDNS config.InstallExternalDNS
	// Decisions records what deploy-time analysis filled in (storage,
	// healthcheck) as an annotation on the Deployment, so later no-build
	// deploys can keep those decisions instead of silently dropping them.
	Decisions string
	// Stack is what the app is written in (detect.Stack), kept as an
	// annotation for listings.
	Stack string
	// RouteTargets are the paths of other apps' addresses this app answers
	// on. The server resolves the hostnames, so this package stays pure.
	RouteTargets []RouteTarget

	// TelemetryEndpoint and TelemetryToken configure a standard OTLP/HTTP
	// metrics exporter without overriding values the app owner set explicitly.
	TelemetryEndpoint string
	TelemetryToken    string
}

// RouteTarget is one resolved entry of donkey.yaml's `routes`.
type RouteTarget struct {
	// App is the app whose address is shared, Host its resolved hostname.
	App         string
	Host        string
	Path        string
	StripPrefix bool
}

// Name is what this app's objects are called. Labels always keep App.Name as
// the logical identity; selectors include the space separately.
func (in Input) Name() string {
	if in.ObjectName != "" {
		return in.ObjectName
	}
	return in.App.Name
}

func PullSecretName(app string) string { return app + "-registry" }

func EnvSecretName(app string) string { return app + "-env" }

func TelemetrySecretName(app string) string { return app + "-telemetry" }

func DataClaimName(app string) string { return app + "-data" }

// SpaceBindingSecretName is the space's canonical binding of one service,
// named after the service's object name — the service owns it.
func SpaceBindingSecretName(service string) string { return service + "-binding" }

// AppBindingSecretName is an app's copy of a service binding, named after
// the app's object name and the service.
func AppBindingSecretName(app, service string) string { return app + "-binding-" + service }

// AppObjects renders the namespace, optional pull secret, data volume,
// deployment and service for an app, in apply order.
func AppObjects(in Input) ([]runtime.Object, error) {
	app := in.App
	objs := []runtime.Object{SpaceHome(in.Space, in.Namespace, in.SharedNamespace)}
	if !in.SharedNamespace {
		objs = append(objs, SpaceMarker(in.Space, in.Namespace))
	}
	if in.IsolateSpace {
		objs = append(objs, SpaceIsolationPolicy(in.Space, in.Namespace, in.SystemNamespace, in.RestrictEgress))
	}

	if in.RegistryUsername != "" && in.RegistryPassword != "" {
		objs = append(objs, pullSecret(in))
	}
	if in.TelemetryEndpoint != "" && in.TelemetryToken != "" {
		objs = append(objs, telemetrySecret(in))
	}
	if app.HasStorage() {
		pvc, err := dataClaim(in)
		if err != nil {
			return nil, err
		}
		objs = append(objs, pvc)
	}
	dep, err := deployment(in)
	if err != nil {
		return nil, err
	}
	objs = append(objs, dep, service(in))
	return append(objs, NetworkObjects(in)...), nil
}

// NetworkObjects renders the gateway's routing entries and, when configured,
// the exact-host Certificate and DNSEndpoint. None depends on the image, which
// lets the console change exposure without a deploy.
func NetworkObjects(in Input) []runtime.Object {
	var objs []runtime.Object
	if in.Domain != "" {
		objs = append(objs, appRoute(in))
		objs = append(objs, AppTrafficObjects(in)...)
	}
	// Paths this app answers on somebody else's address. The hostname
	// belongs to the app that owns it, but each route carries its own login
	// setting, so a public API can live under a path of a private frontend.
	for _, r := range in.RouteTargets {
		objs = append(objs, PathRoute(in, r))
	}
	return objs
}

// SpaceHome is the object that has to exist before anything of the space
// does: the namespace donkey owns, or — where the whole installation shares a
// namespace it was given — the marker that records the space instead.
func SpaceHome(space, namespace string, shared bool) runtime.Object {
	if shared {
		return SpaceMarker(space, namespace)
	}
	return Namespace(space, namespace)
}

// Namespace is the home of one space: every app of the space lives in it and
// they reach each other by plain service name. Pod Security Standards are
// enforced at "baseline" — enough to keep app pods from going privileged,
// while still running the many real-world images that want to be root.
func Namespace(space, name string) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: func() map[string]string {
				labels := SpaceLabels(space)
				labels["pod-security.kubernetes.io/enforce"] = "baseline"
				labels["pod-security.kubernetes.io/warn"] = "restricted"
				return labels
			}(),
			Annotations: map[string]string{AnnotationManaged: "true"},
		},
	}
}

// dataClaim is the app's one and only volume. Every path in storage.paths
// becomes a folder on it (see storageMounts), which keeps a multi-directory
// app on a single disk — one thing to size, back up and reason about.
func dataClaim(in Input) (*corev1.PersistentVolumeClaim, error) {
	size := in.App.Storage.Size
	if size == "" {
		size = config.DefaultStorage
	}
	q, err := resource.ParseQuantity(size)
	if err != nil {
		return nil, fmt.Errorf("storage.size %q is not a valid size (try 5Gi)", size)
	}
	pvc := &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DataClaimName(in.Name()),
			Namespace: in.Namespace,
			Labels:    Labels(in.Space, in.App.Name, RoleStorage),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: q},
			},
		},
	}
	if class := in.App.Storage.Class; class != "" {
		pvc.Spec.StorageClassName = &class
	}
	return pvc, nil
}

// storageMounts maps each configured path onto its own folder of the single
// volume. The folder name is derived from the path and never changes, so the
// data stays where it was written.
func storageMounts(app *config.AppConfig) []corev1.VolumeMount {
	var mounts []corev1.VolumeMount
	for _, p := range app.Storage.Paths {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      storageVolumeName,
			MountPath: path.Clean(p),
			SubPath:   config.StorageSubPath(p),
		})
	}
	return mounts
}

func pullSecret(in Input) *corev1.Secret {
	return RegistryPullSecret(
		PullSecretName(in.Name()), in.Namespace,
		Labels(in.Space, in.App.Name, RoleRegistryAuth),
		in.RegistryHost, in.RegistryUsername, in.RegistryPassword,
	)
}

func telemetrySecret(in Input) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      TelemetrySecretName(in.Name()),
			Namespace: in.Namespace,
			Labels:    Labels(in.Space, in.App.Name, RoleConfig),
		},
		StringData: map[string]string{
			"OTEL_SERVICE_NAME":             in.App.Name,
			"OTEL_RESOURCE_ATTRIBUTES":      "service.namespace=" + in.Space,
			"OTEL_EXPORTER_OTLP_ENDPOINT":   in.TelemetryEndpoint,
			"OTEL_EXPORTER_OTLP_PROTOCOL":   "http/protobuf",
			"OTEL_EXPORTER_OTLP_HEADERS":    "x-donkey-telemetry-token=" + in.TelemetryToken,
			"OTEL_METRICS_EXPORTER":         "otlp",
			"OTEL_TRACES_EXPORTER":          "none",
			"OTEL_LOGS_EXPORTER":            "none",
			"OTEL_SEMCONV_STABILITY_OPT_IN": "http",
		},
	}
}

// RegistryPullSecret creates the docker config used by app and addon pods.
// Keeping this shape in one place matters: both pull images from the same
// enterprise registry and must encode its host and credentials identically.
func RegistryPullSecret(name, namespace string, labels map[string]string, host, username, password string) *corev1.Secret {
	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	cfg := map[string]any{
		"auths": map[string]any{
			host: map[string]string{
				"username": username,
				"password": password,
				"auth":     auth,
			},
		},
	}
	// json.Marshal cannot fail for this fixed tree of string maps.
	b, _ := json.Marshal(cfg)
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: b},
	}
}

// appEnvironment is everything the app is handed at runtime: its own env
// secret, its addon bindings, and the env block from donkey.yaml.
func appEnvironment(in Input) ([]corev1.EnvVar, []corev1.EnvFromSource) {
	app := in.App
	optional := true
	var envFrom []corev1.EnvFromSource
	if in.TelemetryEndpoint != "" && in.TelemetryToken != "" {
		envFrom = append(envFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: TelemetrySecretName(in.Name())},
			},
		})
	}
	envFrom = append(envFrom, corev1.EnvFromSource{
		SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: EnvSecretName(in.Name())},
			Optional:             &optional,
		},
	})
	for _, name := range in.AddonSecretNames {
		envFrom = append(envFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: name},
			},
		})
	}

	// PORT is always provided (Heroku convention); an explicit env entry wins.
	var env []corev1.EnvVar
	if _, ok := app.Env["PORT"]; !ok {
		env = append(env, corev1.EnvVar{Name: "PORT", Value: fmt.Sprintf("%d", app.Port)})
	}
	for _, k := range slices.Sorted(maps.Keys(app.Env)) {
		service, key, ref := config.ParseEnvRef(app.Env[k])
		if !ref {
			env = append(env, corev1.EnvVar{Name: k, Value: app.Env[k]})
			continue
		}
		env = append(env, corev1.EnvVar{Name: k, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: AppBindingSecretName(in.Name(), service)},
				Key:                  key,
			},
		}})
	}
	return env, envFrom
}

func deployment(in Input) (*appsv1.Deployment, error) {
	app := in.App
	res, err := resources(app.Resources)
	if err != nil {
		return nil, err
	}

	env, envFrom := appEnvironment(in)

	// With a configured path we probe over HTTP; otherwise the zero-config
	// default is "does the port answer" — safe for apps with no route on /.
	handler := corev1.ProbeHandler{
		TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(int32(app.Port))},
	}
	if app.Healthcheck.Path != "" {
		handler = corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: app.Healthcheck.Path,
				Port: intstr.FromInt32(int32(app.Port)),
			},
		}
	}
	probe := &corev1.Probe{ProbeHandler: handler}

	container := corev1.Container{
		Name:           WebContainerName,
		Image:          in.ImageRef,
		Command:        app.Command,
		Ports:          []corev1.ContainerPort{{ContainerPort: int32(app.Port), Name: "http"}},
		Env:            env,
		EnvFrom:        envFrom,
		ReadinessProbe: probe,
		// The startup probe gives slow starters (JVM warmup, migrations
		// before listen) four minutes of grace; only then does the liveness
		// probe take over — without this, liveness kills an app that needs
		// more than ~55s and the deploy reads as "keeps crashing".
		StartupProbe: &corev1.Probe{
			ProbeHandler:     probe.ProbeHandler,
			PeriodSeconds:    5,
			FailureThreshold: 48,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler:  probe.ProbeHandler,
			PeriodSeconds: 15,
		},
		Resources: res,
	}

	// App code is untrusted by definition: it gets no Kubernetes credential,
	// cannot gain privileges, and cannot open raw sockets. Keep the runtime's
	// other conventional container capabilities, though: stock images such as
	// nginx use CHOWN/SETUID/SETGID during their root-to-worker startup, and the
	// official Caddy binary carries NET_BIND_SERVICE as a file capability.
	container.SecurityContext = &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"NET_RAW"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	spec := corev1.PodSpec{
		Containers:                   []corev1.Container{container},
		AutomountServiceAccountToken: new(false),
		EnableServiceLinks:           new(false),
	}
	if in.RegistryUsername != "" && in.RegistryPassword != "" {
		spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: PullSecretName(in.Name())}}
	}

	annotations := map[string]string{AnnotationImage: in.ImageRef}
	if in.App.IsAPI() {
		annotations[AnnotationMode] = config.AppModeAPI
	}
	if in.App.CORS != "" {
		annotations[AnnotationCORS] = in.App.CORS
	}
	if in.Decisions != "" {
		annotations[AnnotationDecisions] = in.Decisions
	}
	if in.Stack != "" {
		annotations[AnnotationStack] = in.Stack
	}
	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        in.Name(),
			Namespace:   in.Namespace,
			Labels:      withVersion(Labels(in.Space, in.App.Name, RoleWorkload), in.ImageRef),
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: app.Replicas,
			Selector: &metav1.LabelSelector{MatchLabels: AppSelector(in.Space, in.App.Name, RoleWorkload)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: withVersion(Labels(in.Space, in.App.Name, RoleWorkload), in.ImageRef)},
			},
		},
	}

	if app.HasStorage() {
		spec.Containers[0].VolumeMounts = storageMounts(app)
		spec.Volumes = []corev1.Volume{{
			Name: storageVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: DataClaimName(in.Name()),
				},
			},
		}}
		// A fresh volume belongs to root while images commonly run as somebody
		// else, so the app would find its data folder read-only. fsGroup is
		// added as a supplementary group and given the volume.
		// OnRootMismatch avoids walking a large volume on every restart.
		fsGroup := storageFSGroup
		onMismatch := corev1.FSGroupChangeOnRootMismatch
		spec.SecurityContext = &corev1.PodSecurityContext{
			FSGroup:             &fsGroup,
			FSGroupChangePolicy: &onMismatch,
		}
		// The disk attaches to one machine at a time: the old copy has to let
		// go before the new one starts.
		one := int32(1)
		dep.Spec.Replicas = &one
		dep.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	}
	dep.Spec.Template.Spec = spec
	return dep, nil
}

func service(in Input) *corev1.Service {
	app := in.App
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      in.Name(),
			Namespace: in.Namespace,
			Labels:    Labels(in.Space, in.App.Name, RoleWorkload),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: AppSelector(in.Space, in.App.Name, RoleWorkload),
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       80,
				TargetPort: intstr.FromInt32(int32(app.Port)),
			}},
		},
	}
}

func resources(r *config.Resources) (corev1.ResourceRequirements, error) {
	out := corev1.ResourceRequirements{}
	var err error
	if r == nil {
		return out, nil
	}
	if out.Requests, err = quantities(r.Requests); err != nil {
		return out, fmt.Errorf("resources.requests: %w", err)
	}
	if out.Limits, err = quantities(r.Limits); err != nil {
		return out, fmt.Errorf("resources.limits: %w", err)
	}
	return out, nil
}

func quantities(m map[string]string) (corev1.ResourceList, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := corev1.ResourceList{}
	for k, v := range m {
		q, err := resource.ParseQuantity(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a valid amount", k, v)
		}
		out[corev1.ResourceName(k)] = q
	}
	return out, nil
}
