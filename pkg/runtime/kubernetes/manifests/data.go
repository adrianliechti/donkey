package manifests

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The control plane's data volume: today every app's source history (one git
// repository per app under source/), and the place for whatever else it comes
// to keep. Its own claim, apart from telemetry's, so it can live on different
// storage — shared NFS in production. donkey-orchestrator alone mounts it: it
// writes the history while deploying and serves every read of it, and as one
// replica rolled with Recreate it never needs the volume on two nodes.
const (
	DataPVCName = "donkey-data"
	// DataMountPath is where the orchestrator finds it.
	DataMountPath = "/var/lib/donkey"
)

// dataClaimName is the claim the orchestrator mounts: the installation's
// own, or the one donkey creates.
func dataClaimName(in SystemInput) string {
	if in.Data.ExistingClaim != "" {
		return in.Data.ExistingClaim
	}
	return DataPVCName
}

// dataVolumeClaim is created only when the installation names no claim of
// its own.
func dataVolumeClaim(in SystemInput) *corev1.PersistentVolumeClaim {
	if in.Data.ExistingClaim != "" {
		return nil
	}
	claim := &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DataPVCName,
			Namespace: in.Namespace,
			Labels:    SystemLabels(OrchestratorName),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
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

// withDataVolume mounts the data volume into the orchestrator and tells it
// where. The claim belongs to the pod's uid: a fresh volume is made writable
// for it, and an already-owned one is left alone.
func withDataVolume(in SystemInput, spec *corev1.PodSpec) {
	container := &spec.Containers[0]
	container.Env = append(container.Env, corev1.EnvVar{Name: "DONKEY_DATA_DIR", Value: DataMountPath})
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "data", MountPath: DataMountPath})
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: dataClaimName(in),
		}},
	})
	fsGroup := systemRunAsUser
	fsPolicy := corev1.FSGroupChangeOnRootMismatch
	spec.SecurityContext.FSGroup = &fsGroup
	spec.SecurityContext.FSGroupChangePolicy = &fsPolicy
}
