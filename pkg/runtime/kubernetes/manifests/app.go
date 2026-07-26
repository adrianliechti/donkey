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
	"regexp"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/adrianliechti/donkey/internal/config"
)

const (
	// ManagedByValue marks every object donkey owns.
	ManagedByValue = "donkey"
	// AnnotationManaged marks namespaces donkey created (and may delete).
	AnnotationManaged = "donkey.dev/managed"
	// AnnotationImage records the deployed image on the Deployment.
	AnnotationImage = "donkey.dev/image"
	// AnnotationDecisions records deploy-time analysis decisions on the
	// Deployment (JSON), so no-build deploys can keep them.
	AnnotationDecisions = "donkey.dev/decisions"
	// AnnotationConsole records network choices made in the admin console
	// (exposure, domain, access). It is deliberately separate from decisions:
	// a deploy rewrites the decisions annotation from donkey.yaml, while a
	// console choice must survive deploys — until donkey.yaml itself settles
	// the matter, which always wins.
	AnnotationConsole = "donkey.dev/console"
	// LabelApp / LabelComponent identify donkey-owned resources;
	// LabelSpace groups the apps of one space (one namespace holds them all).
	LabelApp       = "donkey.dev/app"
	LabelComponent = "donkey.dev/component"
	LabelSpace     = "donkey.dev/space"
	// LabelShared marks an app's binding Secret as a copy of a space-shared
	// addon's — removing the shared addon has to find (only) these copies.
	LabelShared = "donkey.dev/shared"

	ComponentWeb          = "web"
	ComponentRegistryAuth = "registry-auth"

	webContainerName = "web"

	// storageVolumeName is the app's single volume; every configured path
	// becomes a folder on it.
	storageVolumeName = "data"
	// storageFSGroup owns that volume so any container user can write to it.
	storageFSGroup = int64(2000)
)

func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Labels marks one object as donkey's. Callers that don't know the space
// (older objects, cluster-wide lookups) pass "" and simply get no space label.
//
// Two sets are written, saying the same things. donkey.dev/* is what donkey
// selects on, so its meaning is fixed by this code and nothing else. The
// app.kubernetes.io/* set is the Kubernetes-recommended vocabulary, and it is
// there so donkey's grouping is legible to tools that never heard of donkey:
// `part-of` is the standard spelling of "these belong to one application",
// which is exactly what a space is, and it is what k9s, Lens, Backstage and
// OpenShift's topology view group their displays by.
//
// These are metadata labels only. The Deployment's own selector is written
// separately from donkey.dev/* alone (see appDeployment) — a selector is
// immutable once created, so adding a label here must never change one.
func Labels(space, app, component string) map[string]string {
	l := map[string]string{
		"app.kubernetes.io/name":       app,
		"app.kubernetes.io/managed-by": ManagedByValue,
		LabelApp:                       app,
		LabelComponent:                 component,
	}
	if component != "" {
		l["app.kubernetes.io/component"] = component
	}
	if space != "" {
		l[LabelSpace] = space
		l["app.kubernetes.io/part-of"] = space
	}
	return l
}

// labelValue matches what Kubernetes accepts as a label value.
var labelValue = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)

// withVersion adds the standard version label, taken from the image's tag.
//
// The tag is not donkey's to choose — `--tag` accepts whatever the user
// passes — and a label value the API server rejects would fail the whole
// apply. So a tag that cannot be a label is simply not published as one: a
// missing label costs a line in `k9s`, a rejected one costs the deploy.
func withVersion(l map[string]string, imageRef string) map[string]string {
	tag := imageRef
	if i := strings.LastIndex(tag, "@"); i >= 0 {
		tag = tag[:i] // a digest is not a version
	}
	slash := strings.LastIndex(tag, "/")
	if i := strings.LastIndex(tag, ":"); i > slash {
		tag = tag[i+1:]
	} else {
		return l
	}
	if len(tag) > 63 || !labelValue.MatchString(tag) {
		return l
	}
	l["app.kubernetes.io/version"] = tag
	return l
}

// Input is everything needed to render an app's core objects.
type Input struct {
	App *config.AppConfig
	// Space is the app's space; Namespace is the namespace that space maps to.
	Space     string
	Namespace string
	ImageRef  string
	// RegistryHost/Username/Password feed the image pull secret; empty
	// username or password skips it (anonymous registry).
	RegistryHost     string
	RegistryUsername string
	RegistryPassword string
	// AddonSecretNames lists binding secrets injected via envFrom.
	AddonSecretNames []string
	// Domain is the hostname the app is served on; empty skips the route.
	Domain string
	// Decisions records what deploy-time analysis filled in (storage,
	// healthcheck) as an annotation on the Deployment, so later no-build
	// deploys can keep those decisions instead of silently dropping them.
	Decisions string
	// RouteTargets are the paths of other apps' addresses this app answers
	// on. The server resolves the hostnames, so this package stays pure.
	RouteTargets []RouteTarget
}

// RouteTarget is one resolved entry of donkey.yaml's `routes`.
type RouteTarget struct {
	// App is the app whose address is shared, Host its resolved hostname.
	App  string
	Host string
	Path string
}

func PullSecretName(app string) string { return app + "-registry" }
func EnvSecretName(app string) string  { return app + "-env" }
func DataClaimName(app string) string  { return app + "-data" }

// AppObjects renders the namespace, optional pull secret, data volume,
// deployment and service for an app, in apply order.
func AppObjects(in Input) ([]runtime.Object, error) {
	app := in.App
	objs := []runtime.Object{Namespace(in.Space, in.Namespace)}

	if in.RegistryUsername != "" && in.RegistryPassword != "" {
		sec, err := pullSecret(in)
		if err != nil {
			return nil, err
		}
		objs = append(objs, sec)
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

// NetworkObjects renders everything that decides how the app is reached —
// the gateway's routing table entries. None of it depends on the image,
// which is what lets the console change an app's exposure without a deploy.
// Certificates need no objects at all: the gateway issues them itself, per
// hostname, the first time one is needed.
func NetworkObjects(in Input) []runtime.Object {
	var objs []runtime.Object
	if in.Domain != "" {
		objs = append(objs, appRoute(in))
	}
	// Paths this app answers on somebody else's address. The hostname
	// belongs to the app that owns it, but each route carries its own login
	// setting, so a public API can live under a path of a private frontend.
	for _, r := range in.RouteTargets {
		objs = append(objs, PathRoute(in, r))
	}
	return objs
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
			Labels: map[string]string{
				"app.kubernetes.io/name":             space,
				"app.kubernetes.io/managed-by":       ManagedByValue,
				LabelSpace:                           space,
				"pod-security.kubernetes.io/enforce": "baseline",
				"pod-security.kubernetes.io/warn":    "restricted",
			},
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
			Name:      DataClaimName(in.App.Name),
			Namespace: in.Namespace,
			Labels:    Labels(in.Space, in.App.Name, ComponentWeb),
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

func pullSecret(in Input) (*corev1.Secret, error) {
	auth := base64.StdEncoding.EncodeToString([]byte(in.RegistryUsername + ":" + in.RegistryPassword))
	cfg := map[string]any{
		"auths": map[string]any{
			in.RegistryHost: map[string]string{
				"username": in.RegistryUsername,
				"password": in.RegistryPassword,
				"auth":     auth,
			},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      PullSecretName(in.App.Name),
			Namespace: in.Namespace,
			Labels:    Labels(in.Space, in.App.Name, ComponentRegistryAuth),
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: b},
	}, nil
}

// appEnvironment is everything the app is handed at runtime: its own env
// secret, its addon bindings, and the env block from donkey.yaml. The
// pre-deploy job uses the identical set, because a migration that cannot see
// DATABASE_URL is worse than no migration at all.
func appEnvironment(in Input) ([]corev1.EnvVar, []corev1.EnvFromSource) {
	app := in.App
	optional := true
	envFrom := []corev1.EnvFromSource{{
		SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: EnvSecretName(app.Name)},
			Optional:             &optional,
		},
	}}
	for _, name := range in.AddonSecretNames {
		envFrom = append(envFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: name},
			},
		})
	}

	// PORT is always provided (Heroku convention) — railpack-built apps and
	// static servers listen on it. An explicit env entry wins.
	var env []corev1.EnvVar
	if _, ok := app.Env["PORT"]; !ok {
		env = append(env, corev1.EnvVar{Name: "PORT", Value: fmt.Sprintf("%d", app.Port)})
	}
	for _, k := range slices.Sorted(maps.Keys(app.Env)) {
		env = append(env, corev1.EnvVar{Name: k, Value: app.Env[k]})
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
		Name:           webContainerName,
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
	// cannot gain privileges, and keeps no capabilities it didn't need.
	container.SecurityContext = &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	automount := false
	spec := corev1.PodSpec{
		Containers:                   []corev1.Container{container},
		AutomountServiceAccountToken: &automount,
	}
	if in.RegistryUsername != "" && in.RegistryPassword != "" {
		spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: PullSecretName(app.Name)}}
	}

	annotations := map[string]string{AnnotationImage: in.ImageRef}
	if in.Decisions != "" {
		annotations[AnnotationDecisions] = in.Decisions
	}
	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        app.Name,
			Namespace:   in.Namespace,
			Labels:      withVersion(Labels(in.Space, app.Name, ComponentWeb), in.ImageRef),
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: app.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{LabelApp: app.Name, LabelComponent: ComponentWeb},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: withVersion(Labels(in.Space, app.Name, ComponentWeb), in.ImageRef)},
			},
		},
	}

	if app.HasStorage() {
		spec.Containers[0].VolumeMounts = storageMounts(app)
		spec.Volumes = []corev1.Volume{{
			Name: storageVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: DataClaimName(app.Name),
				},
			},
		}}
		// A fresh volume belongs to root, while images commonly run as
		// somebody else — the app would find its own data folder read-only.
		// Kubernetes adds fsGroup as a supplementary group of every container
		// process and gives the volume to that group, so the app can write
		// whichever user it runs as. OnRootMismatch keeps restarts quick by
		// not walking a large volume every time.
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
			Name:      app.Name,
			Namespace: in.Namespace,
			Labels:    Labels(in.Space, app.Name, ComponentWeb),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{LabelApp: app.Name, LabelComponent: ComponentWeb},
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
