package manifests

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	TelemetryAppName  = "donkey-telemetry"
	telemetryOTLPPort = int32(4318)
	telemetryAPIPort  = int32(9090)
	telemetryDataPath = "/var/lib/donkey"
	telemetryDBPath   = telemetryDataPath + "/telemetry.duckdb"
	telemetryPVCName  = "donkey-telemetry-data"
)

func telemetryObjects(in SystemInput) []runtime.Object {
	return []runtime.Object{
		telemetryClaim(in),
		telemetryDeployment(in),
		telemetryService(in),
		telemetryNetworkPolicy(in),
	}
}

func TelemetryOTLPEndpoint(namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", TelemetryAppName, namespace, telemetryOTLPPort)
}

func TelemetryAPIEndpoint(namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", TelemetryAppName, namespace, telemetryAPIPort)
}

func telemetryClaim(in SystemInput) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      telemetryPVCName,
			Namespace: in.Namespace,
			Labels:    systemLabels(TelemetryAppName),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("2Gi"),
			}},
		},
	}
}

func telemetryDeployment(in SystemInput) *appsv1.Deployment {
	labels := systemLabels(TelemetryAppName)
	replicas := int32(1)
	fsGroup := systemRunAsUser
	fsPolicy := corev1.FSGroupChangeOnRootMismatch
	automount := false
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: TelemetryAppName, Namespace: in.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			// DuckDB is embedded and one process owns its read-write file. Recreate
			// prevents an upgrade from briefly starting two writers.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: in.hashedTemplateMeta(TelemetryAppName, configHash(in.TelemetryKey)),
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: &automount,
					PriorityClassName:            systemPriorityClass,
					ImagePullSecrets:             systemImagePullSecrets(in),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:        new(true),
						RunAsUser:           new(systemRunAsUser),
						RunAsGroup:          new(systemRunAsUser),
						FSGroup:             &fsGroup,
						FSGroupChangePolicy: &fsPolicy,
					},
					Containers: []corev1.Container{{
						Name:            "telemetry",
						Image:           in.ServerImage,
						ImagePullPolicy: imagePullPolicy(in.ServerImage),
						Args:            []string{"telemetry"},
						Env: []corev1.EnvVar{
							{Name: "DONKEY_TELEMETRY_LISTEN", Value: fmt.Sprintf(":%d", telemetryOTLPPort)},
							{Name: "DONKEY_TELEMETRY_API_LISTEN", Value: fmt.Sprintf(":%d", telemetryAPIPort)},
							{Name: "DONKEY_TELEMETRY_DB", Value: telemetryDBPath},
							secretEnvVar("DONKEY_TELEMETRY_KEY", "telemetry-key"),
						},
						Ports: []corev1.ContainerPort{
							{Name: "otlp-http", ContainerPort: telemetryOTLPPort},
							{Name: "api", ContainerPort: telemetryAPIPort},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: telemetryDataPath}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
							Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
						},
						SecurityContext: systemSecurityContext(),
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
							Path: "/readyz", Port: intstr.FromInt32(telemetryAPIPort),
						}}},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
								Path: "/healthz", Port: intstr.FromInt32(telemetryAPIPort),
							}},
							PeriodSeconds: 15,
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "data",
						VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: telemetryPVCName,
						}},
					}},
				},
			},
		},
	}
}

func telemetryService(in SystemInput) *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: TelemetryAppName, Namespace: in.Namespace, Labels: systemLabels(TelemetryAppName)},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: systemLabels(TelemetryAppName),
			Ports: []corev1.ServicePort{
				{Name: "otlp-http", Port: telemetryOTLPPort, TargetPort: intstr.FromInt32(telemetryOTLPPort)},
				{Name: "api", Port: telemetryAPIPort, TargetPort: intstr.FromInt32(telemetryAPIPort)},
			},
		},
	}
}

func telemetryNetworkPolicy(in SystemInput) *networkPolicy {
	tcp := corev1.ProtocolTCP
	otlp := intstr.FromInt32(telemetryOTLPPort)
	api := intstr.FromInt32(telemetryAPIPort)
	metricPeers := append([]networkingv1.NetworkPolicyPeer{systemPeer(in, GatewayAppName)}, appPeers(in)...)
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      TelemetryAppName,
			Namespace: in.Namespace,
			Labels:    systemLabels(TelemetryAppName),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: systemLabels(TelemetryAppName)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{From: metricPeers, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &otlp}}},
				{From: []networkingv1.NetworkPolicyPeer{systemPeer(in, ServerName)}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &api}}},
			},
		},
	}
}
