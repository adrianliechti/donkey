package addons

import (
	"fmt"
	"net/url"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/adrianliechti/donkey/internal/k8s/manifests"
)

const (
	pbPort = 8090
	// PocketBase requires a syntactically valid email for its superuser; the
	// address is never mailed, it is only the login name of the account.
	pbAdminEmail = "admin@donkey.local"
)

// pocketbase publishes no official container image; this community image
// repacks the upstream release binaries and mirrors its version tags.
const pbImage = "ghcr.io/muchobien/pocketbase"

type pocketbase struct{}

func (pocketbase) Type() string { return "pocketbase" }
func (pocketbase) Description() string {
	return "Accounts, file uploads and a records API in one small server."
}
func (pocketbase) CredentialKey() string { return "POCKETBASE_ADMIN_PASSWORD" }

// DefaultVersion tracks a minor release line, so provisioned instances pick
// up patch releases on restart without jumping minors unasked.
func (pocketbase) DefaultVersion() string { return "0.39" }
func (pocketbase) DefaultStorage() string { return "1Gi" }

func (pocketbase) Binding(in Input) map[string]string {
	// The service is named plainly after itself, so siblings in the space
	// reach it as "pocketbase" — the same way they reach each other.
	host := fmt.Sprintf("%s.%s.svc", in.Name, in.Namespace)
	return map[string]string{
		"POCKETBASE_URL":            fmt.Sprintf("http://%s:%d", host, pbPort),
		"POCKETBASE_ADMIN_EMAIL":    pbAdminEmail,
		"POCKETBASE_ADMIN_PASSWORD": in.Password,
	}
}

func (p pocketbase) Objects(in Input) ([]runtime.Object, error) {
	name := in.Name
	labels := manifests.Labels(in.Space, in.App, Component(in.Name))
	selector := map[string]string{
		manifests.LabelApp:       in.App,
		manifests.LabelComponent: Component(in.Name),
	}

	storage, err := resource.ParseQuantity(in.Storage)
	if err != nil {
		return nil, fmt.Errorf("storage %q is not a valid size (try 1Gi)", in.Storage)
	}
	pvc := &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-data",
			Namespace: in.Namespace,
			Labels:    labels,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: storage},
			},
		},
	}

	replicas := int32(1)
	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: in.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			// Recreate: PocketBase embeds its database (SQLite on the volume),
			// so two pods must never share it during a rollout.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					// A backing service needs no Kubernetes credential.
					AutomountServiceAccountToken: new(false),
					// The image defaults to root; an ordinary user with the
					// volume group-owned is all PocketBase needs.
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: new(true),
						RunAsUser:    new(int64(1000)),
						RunAsGroup:   new(int64(1000)),
						FSGroup:      new(int64(1000)),
					},
					Containers: []corev1.Container{{
						Name: "pocketbase",
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: new(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						Image: pbImage + ":" + in.Version,
						Ports: []corev1.ContainerPort{{ContainerPort: pbPort, Name: "http"}},
						// The image's entrypoint creates (or updates) the
						// superuser from these before serving on 8090 with
						// /pb_data as its data dir.
						Env: []corev1.EnvVar{
							{Name: "PB_ADMIN_EMAIL", ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: SecretName(in.App, in.Name)},
									Key:                  "POCKETBASE_ADMIN_EMAIL",
								},
							}},
							{Name: "PB_ADMIN_PASSWORD", ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: SecretName(in.App, in.Name)},
									Key:                  "POCKETBASE_ADMIN_PASSWORD",
								},
							}},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "data",
							MountPath: "/pb_data",
						}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/api/health", Port: intstr.FromInt32(pbPort)},
							},
							PeriodSeconds: 5,
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "data",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name + "-data"},
						},
					}},
				},
			},
		},
	}

	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: in.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selector,
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       pbPort,
				TargetPort: intstr.FromInt32(pbPort),
			}},
		},
	}

	return []runtime.Object{pvc, dep, svc}, nil
}

func (pocketbase) ExternalBinding(raw string) (map[string]string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("that doesn't look like a PocketBase address — expected https://pocketbase.example.com")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("the PocketBase address names no host — expected https://pocketbase.example.com")
	}
	return map[string]string{
		"POCKETBASE_URL": strings.TrimRight(raw, "/"),
	}, nil
}
