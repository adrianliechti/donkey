package manifests

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// donkey-ai is the openai addon's endpoint — the one donkey endpoint tenant
// code is *meant* to reach. It runs as its own workload, with its own
// identity, because of what that means: a flaw in the part apps can talk to
// must not sit in the process that can write to the cluster. It holds the
// platform's AI credential and read-only access to the binding Secrets, and
// nothing else — no session key, no registry credential, no write verbs.
const (
	AIAppName = "donkey-ai"
	aiPort    = 8089
)

// aiObjects renders the AI endpoint. Every installation gets one: the openai
// addon is in the catalog whether or not a space has connected it yet.
func aiObjects(in SystemInput) []runtime.Object {
	objs := []runtime.Object{aiServiceAccount(in)}
	objs = append(objs, aiTokenAccess(in)...)
	return append(objs,
		aiDeployment(in),
		systemService(in, AIAppName, 80, aiPort),
		aiNetworkPolicy(in),
	)
}

func aiServiceAccount(in SystemInput) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: AIAppName, Namespace: in.Namespace, Labels: SystemLabels(AIAppName)},
	}
}

// aiTokenAccess is all donkey-ai may do: read the Secrets the addon bindings
// live in, to know which per-space tokens are currently valid. Read-only, and
// namespaced wherever the arrangement allows it — with one namespace for
// every app there is exactly one place to look, so this grants nothing
// cluster-wide at all.
func aiTokenAccess(in SystemInput) []runtime.Object {
	rules := []rbacv1.PolicyRule{{
		APIGroups: []string{""},
		Resources: []string{"secrets"},
		Verbs:     []string{"get", "list", "watch"},
	}}
	if !in.Shared() {
		// A namespace per space: the bindings are spread across all of them,
		// and no Role can span namespaces that do not exist yet.
		return []runtime.Object{
			&rbacv1.ClusterRole{
				TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
				ObjectMeta: metav1.ObjectMeta{
					Name:   clusterScopedName(AIAppName, in.Namespace),
					Labels: SystemLabels(AIAppName),
				},
				Rules: rules,
			},
			&rbacv1.ClusterRoleBinding{
				TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
				ObjectMeta: metav1.ObjectMeta{
					Name:   clusterScopedName(AIAppName, in.Namespace),
					Labels: SystemLabels(AIAppName),
				},
				RoleRef: rbacv1.RoleRef{
					APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole",
					Name: clusterScopedName(AIAppName, in.Namespace),
				},
				Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: AIAppName, Namespace: in.Namespace}},
			},
		}
	}
	return []runtime.Object{
		&rbacv1.Role{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
			ObjectMeta: metav1.ObjectMeta{Name: AIAppName, Namespace: in.WorkloadNamespace, Labels: SystemLabels(AIAppName)},
			Rules:      rules,
		},
		&rbacv1.RoleBinding{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
			ObjectMeta: metav1.ObjectMeta{Name: AIAppName, Namespace: in.WorkloadNamespace, Labels: SystemLabels(AIAppName)},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: AIAppName},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: AIAppName, Namespace: in.Namespace}},
		},
	}
}

// aiConfigHash rolls the pod when the credential it swaps in changes.
func (in SystemInput) aiConfigHash() string { return configHash(in.OpenAIAPIKey) }

func aiDeployment(in SystemInput) *appsv1.Deployment {
	labels := SystemLabels(AIAppName)
	replicas := int32(1)
	health := corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(aiPort)},
	}
	env := []corev1.EnvVar{
		{Name: "DONKEY_LISTEN", Value: fmt.Sprintf(":%d", aiPort)},
		{Name: "OPENAI_BASE_URL", Value: in.OpenAIBaseURL},
		secretEnvVar("OPENAI_API_KEY", "OPENAI_API_KEY"),
		// Where to look for the binding Secrets. Only the namespaces — this
		// process never maps a space onto one.
		{Name: "DONKEY_SYSTEM_NAMESPACE", Value: in.Namespace},
		{Name: "DONKEY_WORKLOAD_NAMESPACE", Value: in.WorkloadNamespace},
	}
	// The upstream is usually outside the cluster, so this pod needs the
	// proxy as much as the others do.
	env = append(env, proxyEnv(in.Build.Proxy, registryHostOf(in.RegistryURL))...)
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: AIAppName, Namespace: in.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: SystemSelector(AIAppName)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: in.hashedTemplateMeta(AIAppName, in.ServerImage, in.aiConfigHash()),
				Spec: corev1.PodSpec{
					ServiceAccountName: AIAppName,
					PriorityClassName:  systemPriorityClass,
					SecurityContext:    systemPodSecurityContext(),
					ImagePullSecrets:   systemImagePullSecrets(in),
					Containers: []corev1.Container{{
						Name:            "ai",
						Image:           in.ServerImage,
						ImagePullPolicy: imagePullPolicy(in.ServerImage),
						Args:            []string{"ai"},
						Env:             env,
						Ports:           []corev1.ContainerPort{{ContainerPort: aiPort, Name: "http"}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
						},
						SecurityContext: systemSecurityContext(),
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(aiPort)},
						}},
						LivenessProbe: &corev1.Probe{ProbeHandler: health, PeriodSeconds: 15, FailureThreshold: 3},
					}},
				},
			},
		},
	}
}

// aiNetworkPolicy admits the apps this endpoint exists for — it is the one
// donkey workload tenant pods may reach, which is exactly why it is not the
// one holding cluster write — and donkey-server, which forwards the console's
// /ai/v1 here so the console's chat stays on its own origin.
func aiNetworkPolicy(in SystemInput) *networkPolicy {
	return ingressPolicy(in, AIAppName, aiPort,
		append(appPeers(in), systemPeer(in.Namespace, ServerName)))
}
