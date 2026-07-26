package kubernetes

import (
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// This file materializes the addons package's runtime-neutral vocabulary on
// Kubernetes: a binding becomes a Secret the app's envFrom picks up, a
// Workload becomes PVC + Deployment + Service. The security posture is the
// same for every addon — an unprivileged user, no escalation, the data
// volume group-owned — so it lives here, not in each addon.

// bindingSecret wraps the env contract in the Secret the app's envFrom picks
// up. Identical shape for provisioned and external addons.
func bindingSecret(space, app, namespace, name string, data map[string]string) *corev1.Secret {
	bytes := map[string][]byte{}
	for k, v := range data {
		bytes[k] = []byte(v)
	}
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      addons.SecretName(app, name),
			Namespace: namespace,
			Labels:    manifests.Labels(space, app, addons.Component(name)),
		},
		Data: bytes,
	}
}

// attachmentSecret is an app's copy of a space-shared addon's binding — the
// same contract, pointing at the shared instance, marked so that removing
// the shared addon can find its copies (and only them).
func attachmentSecret(space, app, namespace, name string, data map[string]string) *corev1.Secret {
	sec := bindingSecret(space, app, namespace, name, data)
	sec.Labels[manifests.LabelShared] = "true"
	return sec
}

// aiProxyURL is where apps reach donkey-server's AI proxy on this runtime.
func aiProxyURL() string {
	return fmt.Sprintf("http://%s.%s.svc/ai/v1", manifests.ServerName, manifests.SystemNamespace)
}

// addonObjects renders one provisioned instance: its data volume, its
// Deployment and its Service — named plainly after itself, so siblings in
// the space reach it as e.g. "postgres" the same way they reach each other.
func addonObjects(in addons.Input, w *addons.Workload) ([]k8sruntime.Object, error) {
	name := in.Name
	labels := manifests.Labels(in.Space, in.App, addons.Component(in.Name))
	selector := map[string]string{
		manifests.LabelApp:       in.App,
		manifests.LabelComponent: addons.Component(in.Name),
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

	// Env in a fixed order (maps iterate randomly, applies must not churn);
	// plain values first, credentials referenced from the binding Secret —
	// never inlined into the pod spec.
	var env []corev1.EnvVar
	for _, k := range sortedKeys(w.Env) {
		env = append(env, corev1.EnvVar{Name: k, Value: w.Env[k]})
	}
	for _, k := range sortedKeys(w.SecretEnv) {
		env = append(env, corev1.EnvVar{Name: k, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: addons.SecretName(in.App, in.Name)},
				Key:                  w.SecretEnv[k],
			},
		}})
	}

	probe := &corev1.Probe{PeriodSeconds: 5}
	switch {
	case w.ReadyHTTPPath != "":
		probe.HTTPGet = &corev1.HTTPGetAction{Path: w.ReadyHTTPPath, Port: intstr.FromInt32(w.Port)}
	case len(w.ReadyCommand) > 0:
		probe.Exec = &corev1.ExecAction{Command: w.ReadyCommand}
	default:
		probe = nil
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
			// Recreate: a single-instance service must never have two pods
			// sharing the volume during a rollout.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					// A backing service needs no Kubernetes credential.
					AutomountServiceAccountToken: new(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: new(true),
						RunAsUser:    new(w.RunAsUser),
						RunAsGroup:   new(w.RunAsUser),
						FSGroup:      new(w.RunAsUser),
					},
					Containers: []corev1.Container{{
						Name: name,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: new(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						Image: w.Image,
						Ports: []corev1.ContainerPort{{ContainerPort: w.Port, Name: w.PortName}},
						Env:   env,
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "data",
							MountPath: w.DataPath,
						}},
						ReadinessProbe: probe,
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
				Name:       w.PortName,
				Port:       w.Port,
				TargetPort: intstr.FromInt32(w.Port),
			}},
		},
	}

	return []k8sruntime.Object{pvc, dep, svc}, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
