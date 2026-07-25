// Package manifests turns donkey configuration into Kubernetes objects.
// Everything here is pure — no I/O, no cluster — so it is golden-testable,
// and `--dry-run` renders exactly what deploy applies.
package manifests

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"monobox.ch/donkey/internal/config"
)

const (
	// ManagedByValue marks every object donkey owns.
	ManagedByValue = "donkey"
	// AnnotationManaged marks namespaces donkey created (and may delete).
	AnnotationManaged = "donkey.dev/managed"
	// AnnotationImage records the deployed image on the Deployment.
	AnnotationImage = "donkey.dev/image"
	// LabelApp / LabelComponent identify donkey-owned resources.
	LabelApp       = "donkey.dev/app"
	LabelComponent = "donkey.dev/component"

	ComponentWeb          = "web"
	ComponentRegistryAuth = "registry-auth"

	webContainerName = "web"
)

func Labels(app, component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       app,
		"app.kubernetes.io/managed-by": ManagedByValue,
		LabelApp:                       app,
		LabelComponent:                 component,
	}
}

// Input is everything needed to render an app's core objects.
type Input struct {
	App       *config.AppConfig
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
	// Scheme is http or https — used for the OIDC redirect URL.
	Scheme string
	// GatewayName/Namespace identify the shared Gateway routes attach to.
	GatewayName      string
	GatewayNamespace string
	// OIDC configures the login protection; an empty issuer (or a public
	// app) means no SecurityPolicy is rendered.
	OIDC OIDCConfig
}

type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
}

func PullSecretName(app string) string { return app + "-registry" }
func EnvSecretName(app string) string  { return app + "-env" }

// AppObjects renders the namespace, optional pull secret, deployment and
// service for an app, in apply order.
func AppObjects(in Input) ([]runtime.Object, error) {
	app := in.App
	objs := []runtime.Object{Namespace(app.Name, in.Namespace)}

	if in.RegistryUsername != "" && in.RegistryPassword != "" {
		sec, err := pullSecret(in)
		if err != nil {
			return nil, err
		}
		objs = append(objs, sec)
	}
	dep, err := deployment(in)
	if err != nil {
		return nil, err
	}
	objs = append(objs, dep, service(in))
	if in.Domain != "" {
		objs = append(objs, httpRoute(in))
		// Login protection: every app gets it when OIDC is configured,
		// unless it opted out with `public: true`.
		if !app.Public && in.OIDC.Issuer != "" {
			objs = append(objs, oidcSecret(in), securityPolicy(in))
		}
	}
	return objs, nil
}

func Namespace(app, name string) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      Labels(app, ComponentWeb),
			Annotations: map[string]string{AnnotationManaged: "true"},
		},
	}
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
			Labels:    Labels(in.App.Name, ComponentRegistryAuth),
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: b},
	}, nil
}

func deployment(in Input) (*appsv1.Deployment, error) {
	app := in.App
	res, err := resources(app.Resources)
	if err != nil {
		return nil, err
	}

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
	for _, k := range sortedKeys(app.Env) {
		env = append(env, corev1.EnvVar{Name: k, Value: app.Env[k]})
	}

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
		LivenessProbe: &corev1.Probe{
			ProbeHandler:        probe.ProbeHandler,
			InitialDelaySeconds: 10,
			PeriodSeconds:       15,
		},
		Resources: res,
	}

	spec := corev1.PodSpec{Containers: []corev1.Container{container}}
	if in.RegistryUsername != "" && in.RegistryPassword != "" {
		spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: PullSecretName(app.Name)}}
	}

	selector := map[string]string{LabelApp: app.Name, LabelComponent: ComponentWeb}
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        app.Name,
			Namespace:   in.Namespace,
			Labels:      Labels(app.Name, ComponentWeb),
			Annotations: map[string]string{AnnotationImage: in.ImageRef},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: app.Replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: Labels(app.Name, ComponentWeb)},
				Spec:       spec,
			},
		},
	}, nil
}

func service(in Input) *corev1.Service {
	app := in.App
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: in.Namespace,
			Labels:    Labels(app.Name, ComponentWeb),
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

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
