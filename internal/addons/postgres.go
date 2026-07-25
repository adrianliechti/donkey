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
	pgUser = "donkey"
	pgDB   = "app"
	pgPort = 5432
)

type postgres struct{}

func (postgres) Type() string           { return "postgres" }
func (postgres) CredentialKey() string  { return "PGPASSWORD" }
func (postgres) DefaultVersion() string { return "16" }
func (postgres) DefaultStorage() string { return "1Gi" }

func (postgres) Binding(in Input) map[string]string {
	host := fmt.Sprintf("%s.%s.svc", InstanceName(in.App, in.Name), in.Namespace)
	return map[string]string{
		"DATABASE_URL": fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
			pgUser, in.Password, host, pgPort, pgDB),
		"PGHOST":     host,
		"PGPORT":     fmt.Sprintf("%d", pgPort),
		"PGUSER":     pgUser,
		"PGPASSWORD": in.Password,
		"PGDATABASE": pgDB,
	}
}

func (p postgres) Objects(in Input) ([]runtime.Object, error) {
	name := InstanceName(in.App, in.Name)
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
			// Recreate: a single-instance database must never have two pods
			// sharing the volume during a rollout.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					// A database needs no Kubernetes credential.
					AutomountServiceAccountToken: new(false),
					Containers: []corev1.Container{{
						Name: "postgres",
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: new(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						Image: "postgres:" + in.Version,
						Ports: []corev1.ContainerPort{{ContainerPort: pgPort, Name: "postgres"}},
						Env: []corev1.EnvVar{
							{Name: "POSTGRES_USER", Value: pgUser},
							{Name: "POSTGRES_DB", Value: pgDB},
							{Name: "PGDATA", Value: "/var/lib/postgresql/data/pgdata"},
							{Name: "POSTGRES_PASSWORD", ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: SecretName(in.App, in.Name)},
									Key:                  "PGPASSWORD",
								},
							}},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "data",
							MountPath: "/var/lib/postgresql/data",
						}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								Exec: &corev1.ExecAction{Command: []string{"pg_isready", "-U", pgUser, "-d", pgDB}},
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
				Name:       "postgres",
				Port:       pgPort,
				TargetPort: intstr.FromInt32(pgPort),
			}},
		},
	}

	return []runtime.Object{pvc, dep, svc}, nil
}

// ExternalPostgresBinding derives the same env contract from a user-supplied
// connection URL, so remote databases bind exactly like provisioned ones.
func ExternalPostgresBinding(raw string) (map[string]string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return nil, fmt.Errorf("that doesn't look like a postgres URL — expected postgres://user:password@host:5432/dbname")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("the postgres URL names no host — expected postgres://user:password@host:5432/dbname")
	}
	if strings.TrimPrefix(u.Path, "/") == "" {
		return nil, fmt.Errorf("the postgres URL names no database — expected postgres://user:password@host:5432/dbname")
	}
	password, _ := u.User.Password()
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	return map[string]string{
		"DATABASE_URL": raw,
		"PGHOST":       u.Hostname(),
		"PGPORT":       port,
		"PGUSER":       u.User.Username(),
		"PGPASSWORD":   password,
		"PGDATABASE":   strings.TrimPrefix(u.Path, "/"),
	}, nil
}
