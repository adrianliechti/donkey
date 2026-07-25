package manifests

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ComponentPreDeploy marks the one-shot job that runs before a new version
// starts serving.
const ComponentPreDeploy = "predeploy"

// PreDeployJobName is deliberately stable: one job per app, replaced on every
// deploy, so nobody has to clean up after a hundred migrations.
func PreDeployJobName(app string) string { return app + "-predeploy" }

// PreDeployJob runs `preDeploy` against the new image before the Deployment
// is updated. It gets the same image, environment, addon bindings and disk as
// the app — a migration has to see exactly what the app will see.
//
// It is a Job rather than an initContainer on purpose: an initContainer runs
// once per pod (so twice on a two-replica rollout, and again on every
// restart), while a migration must run exactly once per deploy.
func PreDeployJob(in Input) *batchv1.Job {
	app := in.App
	env, envFrom := appEnvironment(in)

	container := corev1.Container{
		Name:    "predeploy",
		Image:   in.ImageRef,
		Command: app.PreDeploy,
		Env:     env,
		EnvFrom: envFrom,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: new(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
	spec := corev1.PodSpec{
		// A job that ended is a job that ended: retrying a half-applied
		// migration is usually worse than stopping and saying so.
		RestartPolicy:                corev1.RestartPolicyNever,
		Containers:                   []corev1.Container{container},
		AutomountServiceAccountToken: new(false),
	}
	if in.RegistryUsername != "" && in.RegistryPassword != "" {
		spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: PullSecretName(app.Name)}}
	}
	// The disk too: a migration may well be the thing that creates the
	// database file the app then opens.
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
		fsGroup := storageFSGroup
		onMismatch := corev1.FSGroupChangeOnRootMismatch
		spec.SecurityContext = &corev1.PodSecurityContext{
			FSGroup:             &fsGroup,
			FSGroupChangePolicy: &onMismatch,
		}
	}

	labels := Labels(in.Space, app.Name, ComponentPreDeploy)
	return &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      PreDeployJobName(app.Name),
			Namespace: in.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: new(int32(0)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       spec,
			},
		},
	}
}
