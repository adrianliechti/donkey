package manifests

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// donkey-server's data volume: today every app's source history (one git
// repository per app under source/), and the place for whatever else the
// server comes to keep. Its own claim, apart from telemetry's, so it can live
// on different storage — shared NFS in production.
const (
	DataPVCName = "donkey-data"
	// DataMountPath is where donkey-server finds it.
	DataMountPath = "/var/lib/donkey"
)

// serverDataClaimName is the claim donkey-server mounts: the installation's
// own, or the one donkey creates.
func serverDataClaimName(in SystemInput) string {
	if in.Data.ExistingClaim != "" {
		return in.Data.ExistingClaim
	}
	return DataPVCName
}

// serverDataClaim is created only when the installation names no claim of
// its own.
func serverDataClaim(in SystemInput) *corev1.PersistentVolumeClaim {
	if !in.Data.Enabled() || in.Data.ExistingClaim != "" {
		return nil
	}
	mode := corev1.ReadWriteOnce
	if in.Data.Shared() {
		mode = corev1.ReadWriteMany
	}
	claim := &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DataPVCName,
			Namespace: in.Namespace,
			Labels:    SystemLabels(ServerName),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{mode},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse(in.Data.ClaimSize()),
			}},
		},
	}
	if in.Data.StorageClass != "" {
		claim.Spec.StorageClassName = &in.Data.StorageClass
	}
	return claim
}

// withDataVolume mounts the data volume into donkey-server. A volume only one
// node can attach also decides the rollout: a replacement scheduled on
// another node could never start while the old pod holds the volume, so the
// old one goes first (deploys in flight still drain before it does). That is
// a rolling update without surge rather than Recreate: an existing
// Deployment cannot switch type in an apply — Kubernetes keeps the rolling
// parameters it defaulted and then refuses them beside Recreate.
func withDataVolume(in SystemInput, spec *corev1.PodSpec, strategy *appsv1.DeploymentStrategy) {
	if !in.Data.Enabled() {
		return
	}
	container := &spec.Containers[0]
	container.Env = append(container.Env, corev1.EnvVar{Name: "DONKEY_DATA_DIR", Value: DataMountPath})
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "data", MountPath: DataMountPath})
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: serverDataClaimName(in),
		}},
	})
	// The claim belongs to the server's uid: a fresh volume is made writable
	// for it, and an already-owned one is left alone.
	fsGroup := systemRunAsUser
	fsPolicy := corev1.FSGroupChangeOnRootMismatch
	spec.SecurityContext.FSGroup = &fsGroup
	spec.SecurityContext.FSGroupChangePolicy = &fsPolicy
	if !in.Data.Shared() {
		none, one := intstr.FromInt32(0), intstr.FromInt32(1)
		strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
		strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{MaxSurge: &none, MaxUnavailable: &one}
	}
}
