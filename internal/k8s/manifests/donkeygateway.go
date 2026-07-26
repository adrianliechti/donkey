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

// The donkey gateway is donkey's own edge proxy: every app route points at
// its Service, it routes by hostname and path, runs the OIDC login where a
// route demands one, and forwards to the app. Auth lives here because the
// Gateway API has no standard auth — this is the piece that keeps donkey
// portable across Gateway implementations.
const (
	GatewayAppName = "donkey-gateway"
	// donkeyGatewayPort serves app traffic; the health port serves probes —
	// separate, because /healthz on the traffic port would shadow every
	// app's own /healthz.
	donkeyGatewayPort       = 8090
	donkeyGatewayHealthPort = 8091
	// GatewayHealthServicePort is the Service port doctor probes.
	GatewayHealthServicePort = 8081
)

// donkeyGatewayObjects renders the edge proxy. Every installation gets one;
// without an OIDC issuer it simply routes and never asks for a login.
func donkeyGatewayObjects(in SystemInput) []runtime.Object {
	return []runtime.Object{
		donkeyGatewayServiceAccount(in),
		donkeyGatewayClusterRole(in),
		donkeyGatewayClusterRoleBinding(in),
		donkeyGatewayDeployment(in),
		donkeyGatewayService(in),
	}
}

func donkeyGatewayServiceAccount(in SystemInput) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: GatewayAppName, Namespace: in.Namespace, Labels: systemLabels(GatewayAppName)},
	}
}

// donkeyGatewayClusterRole is read-only and narrow: the gateway builds its
// routing table from donkey's HTTPRoutes and touches nothing else.
func donkeyGatewayClusterRole(in SystemInput) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: GatewayAppName, Labels: systemLabels(GatewayAppName)},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"gateway.networking.k8s.io"},
			Resources: []string{"httproutes"},
			Verbs:     []string{"get", "list", "watch"},
		}},
	}
}

func donkeyGatewayClusterRoleBinding(in SystemInput) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: GatewayAppName, Labels: systemLabels(GatewayAppName)},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: GatewayAppName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: GatewayAppName, Namespace: in.Namespace}},
	}
}

// donkeyGatewayConfigHash covers the secret-backed env vars (read only at pod
// creation), so a rotated client secret or session key rolls the pod.
func (in SystemInput) donkeyGatewayConfigHash() string {
	return configHash(in.OIDCClientID, in.OIDCClientSecret, in.SessionKey)
}

func donkeyGatewayDeployment(in SystemInput) *appsv1.Deployment {
	labels := systemLabels(GatewayAppName)
	replicas := int32(1)
	secretEnv := func(name, key string) corev1.EnvVar {
		return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "donkey-system-config"},
				Key:                  key,
			},
		}}
	}
	env := []corev1.EnvVar{
		{Name: "DONKEY_LISTEN", Value: fmt.Sprintf(":%d", donkeyGatewayPort)},
		{Name: "DONKEY_HEALTH_LISTEN", Value: fmt.Sprintf(":%d", donkeyGatewayHealthPort)},
		{Name: "DONKEY_OIDC_ISSUER", Value: in.OIDCIssuer},
		// The central callback lives on the base domain itself; hostnames
		// under it share it (and one session cookie — SSO), custom domains
		// outside keep per-host callbacks.
		{Name: "DONKEY_DOMAIN", Value: in.Domain},
		secretEnv("DONKEY_OIDC_CLIENT_ID", "oidc-client-id"),
		secretEnv("DONKEY_OIDC_CLIENT_SECRET", "oidc-client-secret"),
		secretEnv("DONKEY_SESSION_KEY", "session-key"),
	}
	health := corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(donkeyGatewayHealthPort)},
	}
	spec := corev1.PodSpec{
		ServiceAccountName: GatewayAppName,
		// Every protected app's traffic rides through this pod — it matters
		// as much as the apps themselves.
		PriorityClassName: systemPriorityClass,
		Containers: []corev1.Container{{
			Name:            "gateway",
			Image:           in.ServerImage,
			ImagePullPolicy: imagePullPolicy(in.ServerImage),
			Args:            []string{"gateway"},
			Env:             env,
			Ports: []corev1.ContainerPort{
				{ContainerPort: donkeyGatewayPort, Name: "http"},
				{ContainerPort: donkeyGatewayHealthPort, Name: "health"},
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("50m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
			SecurityContext: &corev1.SecurityContext{
				RunAsNonRoot:             ptrTo(true),
				AllowPrivilegeEscalation: ptrTo(false),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(donkeyGatewayHealthPort)},
			}},
			LivenessProbe: &corev1.Probe{ProbeHandler: health, PeriodSeconds: 15, FailureThreshold: 3},
		}},
	}
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: GatewayAppName, Namespace: in.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: in.hashedTemplateMeta(GatewayAppName, in.donkeyGatewayConfigHash()),
				Spec:       spec,
			},
		},
	}
}

// donkeyGatewayService is what protected routes reference. Sessions live in
// the sealed cookie, so scaling this Service needs nothing but replicas.
func donkeyGatewayService(in SystemInput) *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: GatewayAppName, Namespace: in.Namespace, Labels: systemLabels(GatewayAppName)},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: systemLabels(GatewayAppName),
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, TargetPort: intstr.FromInt32(donkeyGatewayPort)},
				{Name: "health", Port: GatewayHealthServicePort, TargetPort: intstr.FromInt32(donkeyGatewayHealthPort)},
			},
		},
	}
}
