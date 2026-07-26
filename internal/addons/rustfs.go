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
	rfPort = 9000
	// RustFS accepts any string as its root access key; a fixed one keeps the
	// binding predictable — the secret key is the credential that matters.
	rfAccessKey = "donkey"
	// RustFS follows the S3 default region; pinning it on both sides (server
	// env and binding) keeps SDK signing from ever guessing.
	rfRegion = "us-east-1"
)

// rustfs publishes an official image; it runs as its own user (uid 10001)
// and serves the S3 API on 9000 with an optional console on 9001, which
// donkey keeps switched off — apps talk S3, humans get the console dialog.
const rfImage = "rustfs/rustfs"

type rustfs struct{}

func (rustfs) Type() string { return "rustfs" }
func (rustfs) Description() string {
	return "S3-compatible object storage — buckets and files, in one small server."
}
func (rustfs) CredentialKey() string { return "AWS_SECRET_ACCESS_KEY" }

// DefaultVersion pins an exact release: upstream is still in its beta line,
// where patch jumps can carry breaking changes a data store must not pick
// up on a restart.
func (rustfs) DefaultVersion() string { return "1.0.0-beta.11" }
func (rustfs) DefaultStorage() string { return "1Gi" }

func (rustfs) Binding(in Input) map[string]string {
	// The service is named plainly after itself, so siblings in the space
	// reach it as "rustfs" — the same way they reach each other.
	endpoint := fmt.Sprintf("http://%s.%s.svc:%d", in.Name, in.Namespace, rfPort)
	return map[string]string{
		// The AWS SDK convention: every S3 client picks these up without a
		// line of configuration.
		"AWS_ACCESS_KEY_ID":     rfAccessKey,
		"AWS_SECRET_ACCESS_KEY": in.Password,
		"AWS_ENDPOINT_URL_S3":   endpoint,
		"AWS_REGION":            rfRegion,
		// The same endpoint once more, for apps that read a single var.
		"S3_URL": endpoint,
	}
}

func (r rustfs) Objects(in Input) ([]runtime.Object, error) {
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
			// Recreate: a single-node object store must never have two pods
			// sharing the volume during a rollout.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					// A backing service needs no Kubernetes credential.
					AutomountServiceAccountToken: new(false),
					// The image ships its own unprivileged user (rustfs,
					// 10001); saying so explicitly and group-owning the
					// volume is all the pod needs.
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: new(true),
						RunAsUser:    new(int64(10001)),
						RunAsGroup:   new(int64(10001)),
						FSGroup:      new(int64(10001)),
					},
					Containers: []corev1.Container{{
						Name: "rustfs",
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: new(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						Image: rfImage + ":" + in.Version,
						Ports: []corev1.ContainerPort{{ContainerPort: rfPort, Name: "s3"}},
						Env: []corev1.EnvVar{
							// One volume is the single-node layout; RustFS
							// initializes it on first start.
							{Name: "RUSTFS_VOLUMES", Value: "/data"},
							{Name: "RUSTFS_REGION", Value: rfRegion},
							// The web console would be a second, unmanaged
							// door into the data — apps speak S3, admins get
							// donkey's own dialog.
							{Name: "RUSTFS_CONSOLE_ENABLE", Value: "false"},
							{Name: "RUSTFS_ACCESS_KEY", ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: SecretName(in.App, in.Name)},
									Key:                  "AWS_ACCESS_KEY_ID",
								},
							}},
							{Name: "RUSTFS_SECRET_KEY", ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: SecretName(in.App, in.Name)},
									Key:                  "AWS_SECRET_ACCESS_KEY",
								},
							}},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "data",
							MountPath: "/data",
						}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(rfPort)},
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
				Name:       "s3",
				Port:       rfPort,
				TargetPort: intstr.FromInt32(rfPort),
			}},
		},
	}

	return []runtime.Object{pvc, dep, svc}, nil
}

// ExternalBinding derives the endpoint half of the contract from an S3
// address. Credentials don't travel in a bare URL — the app keeps whatever
// keys it already has, same as an external PocketBase keeps its login.
func (rustfs) ExternalBinding(raw string) (map[string]string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("that doesn't look like an S3 endpoint — expected https://s3.example.com")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("the S3 endpoint names no host — expected https://s3.example.com")
	}
	// The trailing slash has to go: SDKs join bucket paths onto it.
	endpoint := strings.TrimRight(raw, "/")
	return map[string]string{
		"AWS_ENDPOINT_URL_S3": endpoint,
		"S3_URL":              endpoint,
	}, nil
}
