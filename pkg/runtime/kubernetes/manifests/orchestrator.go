package manifests

import (
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/adrianliechti/donkey/internal/config"
)

// donkey-orchestrator is the control plane's second process (the same image,
// started with `orchestrator`): it reads uploads, builds them with BuildKit,
// pushes, applies and waits for rollouts. donkey-server forwards plans,
// deploys and the changes serialized with them here, so the API itself has
// nothing long-running left and rolls in seconds.

const (
	// OrchestratorName is donkey-orchestrator's Deployment, Service and
	// ServiceAccount. Installers wait for it like every other workload.
	OrchestratorName = "donkey-orchestrator"
	orchestratorPort = int32(8087)
)

// orchestratorGracePeriod covers the drain (config.DeployDrainTimeout) and a
// minute for the shutdown after it, so the kubelet never kills a build the
// orchestrator is deliberately finishing.
var orchestratorGracePeriod = int64((config.DeployDrainTimeout + time.Minute) / time.Second)

// OrchestratorEndpoint is where donkey-server reaches the orchestrator.
func OrchestratorEndpoint(namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc:80", OrchestratorName, namespace)
}

func orchestratorObjects(in SystemInput) []runtime.Object {
	return []runtime.Object{
		orchestratorDeployment(in),
		systemService(in, OrchestratorName, 80, orchestratorPort),
		orchestratorNetworkPolicy(in),
	}
}

func orchestratorServiceAccount(in SystemInput) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: OrchestratorName, Namespace: in.Namespace, Labels: SystemLabels(OrchestratorName)},
	}
}

// orchestratorDeployment is one replica rolled with Recreate. Its plan store
// and per-app deploy locks are process memory, so two orchestrators at once
// would let two deploys of one app interleave; Recreate starts the new pod
// only once the old one has drained. The pod mounts nothing persistent: a
// volume goes into volumes below, next to the registry Secrets.
func orchestratorDeployment(in SystemInput) *appsv1.Deployment {
	labels := withVersion(SystemLabels(OrchestratorName), in.ServerImage)
	replicas := int32(1)
	grace := orchestratorGracePeriod
	mounts, volumes := controlPlaneVolumes(in)
	health := corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(orchestratorPort)},
	}
	spec := corev1.PodSpec{
		ServiceAccountName:            OrchestratorName,
		PriorityClassName:             systemPriorityClass,
		SecurityContext:               systemPodSecurityContext(),
		ImagePullSecrets:              systemImagePullSecrets(in),
		TerminationGracePeriodSeconds: &grace,
		Containers: []corev1.Container{{
			Name:            "orchestrator",
			ImagePullPolicy: imagePullPolicy(in.ServerImage),
			Image:           in.ServerImage,
			Args:            []string{"orchestrator"},
			// The OIDC client stays with the API: the orchestrator never signs
			// anybody in, it only verifies the token the API forwards.
			Env:             controlPlaneEnv(in, orchestratorPort),
			Ports:           []corev1.ContainerPort{{ContainerPort: orchestratorPort}},
			Resources:       orchestratorResources(),
			SecurityContext: systemSecurityContext(),
			VolumeMounts:    mounts,
			// Readiness asks /readyz, which starts failing the moment the pod
			// begins draining — that is what takes it out of the Service while
			// it finishes the deploys it already accepted.
			ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(orchestratorPort)},
			}},
			// /healthz keeps answering while the orchestrator drains, so this
			// never cuts a drain short.
			LivenessProbe: &corev1.Probe{ProbeHandler: health, PeriodSeconds: 15, FailureThreshold: 3},
		}},
		Volumes: volumes,
	}
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: OrchestratorName, Namespace: in.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: SystemSelector(OrchestratorName)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: in.hashedTemplateMeta(OrchestratorName, in.ServerImage, in.orchestratorConfigHash()),
				Spec:       spec,
			},
		},
	}
}

// orchestratorNetworkPolicy admits donkey-server and nothing else. The
// orchestrator also refuses any request without the server's credential;
// this is the lock in front of that one.
func orchestratorNetworkPolicy(in SystemInput) *networkPolicy {
	return ingressPolicy(in, OrchestratorName, orchestratorPort, []networkingv1.NetworkPolicyPeer{
		systemPeer(in.Namespace, ServerName),
	})
}
