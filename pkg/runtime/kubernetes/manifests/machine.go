package manifests

import (
	"fmt"
	"maps"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/adrianliechti/donkey/internal/config"
)

// Personal machines. A machine is one person's development container: it
// belongs to a user, never to a space, so its objects carry donkey.local/machine
// where an app's carry donkey.local/space and donkey.local/app — which is
// also what keeps them out of every list and sweep written for apps.
//
// Machines live in a namespace of their own, named after the one space name
// nobody can take (<prefix>platform), or beside the apps where the
// installation was given a single namespace. Either way one NetworkPolicy
// fences all of them: nothing reaches a machine except the gateway.
const (
	// LabelMachine is a machine's ID (config.MachineID).
	LabelMachine = "donkey.local/machine"

	// RoleMachine is the machine's Deployment, Service and pods.
	RoleMachine = "machine"
	// RoleMachineHome is the namespace machines live in and the fence around
	// them.
	RoleMachineHome    = "machines"
	RoleMachineConfig  = "machine-config"
	RoleMachineStorage = "machine-storage"
	RoleMachineRoute   = "machine-route"

	// MachineContainerName is the one container of a machine's pod.
	MachineContainerName = "machine"
	// MachinePolicyName is the NetworkPolicy fencing every machine of the
	// namespace.
	MachinePolicyName = "donkey-machines"

	// AnnotationMachineOwner says whose machine it is, for the people who
	// run the cluster. Access is never decided from it.
	AnnotationMachineOwner = "donkey.local/machine-owner"
	// AnnotationSignInExpires is when the sign-in the machine holds runs out
	// (RFC 3339).
	AnnotationSignInExpires = "donkey.local/sign-in-expires"
)

// MachineInput is everything needed to render one machine.
type MachineInput struct {
	ID string
	// Namespace is where machines live; SharedNamespace says donkey was given
	// it rather than made it.
	Namespace       string
	SharedNamespace bool
	SystemNamespace string
	// ObjectName is what the machine's objects are called: the ID, qualified
	// where one namespace holds everything.
	ObjectName string
	Image      string
	Host       string
	// Owner is the IdP subject the route admits; OwnerLabel what operators
	// read.
	Owner      string
	OwnerLabel string
	// SignInExpires (RFC 3339) is recorded for the console; empty omits it.
	SignInExpires string
	// Env is the machine's whole environment, credentials included. It only
	// ever exists as a Secret.
	Env      map[string]string
	Settings config.MachineSettings
	// RestrictEgress confines machines to DNS and the control plane.
	RestrictEgress bool
	// ImagePullSecrets are existing secrets of the namespace; the Registry
	// fields add one for the installation's own registry.
	ImagePullSecrets []string
	RegistryHost     string
	RegistryUsername string
	RegistryPassword string
	TLS              config.InstallTLS
	ExternalDNS      config.InstallExternalDNS
}

// MachineLabels marks one object of one machine.
func MachineLabels(id, role string) map[string]string {
	l := baseLabels("", id, role)
	l[LabelPartOf] = ManagedByValue
	l[LabelMachine] = id
	return l
}

// MachineSelector is the immutable identity a machine's Deployment and
// Service share.
func MachineSelector(id string) map[string]string {
	return map[string]string{LabelMachine: id, LabelRole: RoleMachine}
}

func machineHomeLabels() map[string]string {
	l := baseLabels("", RoleMachineHome, RoleMachineHome)
	l[LabelPartOf] = ManagedByValue
	return l
}

func MachineEnvSecretName(name string) string  { return name + "-machine" }
func MachineDataClaimName(name string) string  { return name + "-data" }
func MachineRouteName(name string) string      { return name + "-route" }
func MachinePullSecretName(name string) string { return name + "-registry" }

// MachineEdgeName names the machine's Certificate and DNS record, which live
// beside the gateway. "platform" is no space, so the name is nobody else's.
func MachineEdgeName(id string) string { return config.MachineSpace + NameSeparator + id }

// MachineBackendURL is where the gateway forwards a machine's traffic.
func MachineBackendURL(name, namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc:80", name, namespace)
}

// MachineNamespace is the home of every machine where donkey makes its own
// namespaces. Pod Security is enforced at "restricted" — one level tighter
// than a space's: a machine runs whatever its owner asks an agent to run, and
// its image is donkey's own, so nothing in it needs more.
func MachineNamespace(name string) *corev1.Namespace {
	labels := machineHomeLabels()
	labels["pod-security.kubernetes.io/enforce"] = "restricted"
	labels["pod-security.kubernetes.io/warn"] = "restricted"
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      labels,
			Annotations: map[string]string{AnnotationManaged: "true"},
		},
	}
}

// MachinePolicy fences every machine of the namespace, whether or not the
// installation isolates its spaces: a machine holds its owner's sign-in.
//
// In: the gateway, on the machine's front door, and nothing else — no app,
// no other machine. Out: DNS, the API its CLI calls and the AI endpoint its
// agent uses, named explicitly because some network plugins do not count
// in-cluster pods as part of an address range. Then the rest of the network,
// except the link-local range where cloud metadata services hand out node
// credentials. restrictEgress drops that last rule, which leaves a machine
// the control plane and nothing beyond it.
func MachinePolicy(namespace, installation string, restrictEgress bool) *networkPolicy {
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	door := intstr.FromInt32(config.MachinePort)
	dns := intstr.FromInt32(dnsPort)
	api := intstr.FromInt32(serverPort)
	ai := intstr.FromInt32(aiPort)
	policy := &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      MachinePolicyName,
			Namespace: namespace,
			Labels:    machineHomeLabels(),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{LabelRole: RoleMachine}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  []networkingv1.NetworkPolicyPeer{systemPeer(installation, GatewayAppName)},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &door}},
			}},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &udp, Port: &dns},
					{Protocol: &tcp, Port: &dns},
				}},
				{
					To:    []networkingv1.NetworkPolicyPeer{systemPeer(installation, ServerName)},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &api}},
				},
				{
					To:    []networkingv1.NetworkPolicyPeer{systemPeer(installation, AIAppName)},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &ai}},
				},
			},
		},
	}
	if !restrictEgress {
		policy.Spec.Egress = append(policy.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{
				{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"169.254.0.0/16"}}},
				{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: []string{"fe80::/10"}}},
			},
		})
	}
	return policy
}

// MachineHomeObjects is what has to exist before any machine does.
func MachineHomeObjects(in MachineInput) []runtime.Object {
	var objs []runtime.Object
	if !in.SharedNamespace {
		objs = append(objs, MachineNamespace(in.Namespace))
	}
	return append(objs, MachinePolicy(in.Namespace, in.SystemNamespace, in.RestrictEgress))
}

// MachineObjects renders one machine in apply order: what the pod needs
// first, the route that makes it reachable last.
func MachineObjects(in MachineInput) ([]runtime.Object, error) {
	objs := MachineHomeObjects(in)
	if in.RegistryUsername != "" && in.RegistryPassword != "" {
		objs = append(objs, RegistryPullSecret(MachinePullSecretName(in.ObjectName), in.Namespace,
			MachineLabels(in.ID, RoleRegistryAuth), in.RegistryHost, in.RegistryUsername, in.RegistryPassword))
	}
	claim, err := machineClaim(in)
	if err != nil {
		return nil, err
	}
	deployment, err := machineDeployment(in)
	if err != nil {
		return nil, err
	}
	objs = append(objs, machineEnvSecret(in), claim, deployment, machineService(in))
	objs = append(objs, MachineTrafficObjects(in)...)
	return append(objs, machineRoute(in)), nil
}

func machineEnvSecret(in MachineInput) *corev1.Secret {
	data := map[string][]byte{}
	for key, value := range in.Env {
		data[key] = []byte(value)
	}
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      MachineEnvSecretName(in.ObjectName),
			Namespace: in.Namespace,
			Labels:    MachineLabels(in.ID, RoleMachineConfig),
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

func machineClaim(in MachineInput) (*corev1.PersistentVolumeClaim, error) {
	size, err := resource.ParseQuantity(in.Settings.StorageSize())
	if err != nil {
		return nil, fmt.Errorf("machines.storage %q is not a valid size (try 10Gi)", in.Settings.StorageSize())
	}
	claim := &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      MachineDataClaimName(in.ObjectName),
			Namespace: in.Namespace,
			Labels:    MachineLabels(in.ID, RoleMachineStorage),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}
	if class := in.Settings.StorageClass; class != "" {
		claim.Spec.StorageClassName = &class
	}
	return claim, nil
}

func machineDeployment(in MachineInput) (*appsv1.Deployment, error) {
	limits, err := quantities(map[string]string{
		"cpu":    in.Settings.CPULimit(),
		"memory": in.Settings.MemoryLimit(),
	})
	if err != nil {
		return nil, fmt.Errorf("machines: %w", err)
	}
	// A machine mostly waits for its owner. It asks for little and may use
	// what the installation allows.
	requests := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("256Mi"),
	}
	for name, limit := range limits {
		if request, ok := requests[name]; ok && limit.Cmp(request) < 0 {
			requests[name] = limit
		}
	}
	health := corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
		Path: machineHealthPath,
		Port: intstr.FromInt32(config.MachinePort),
	}}
	user, onMismatch := config.MachineUser, corev1.FSGroupChangeOnRootMismatch
	container := corev1.Container{
		Name:  MachineContainerName,
		Image: in.Image,
		// A machine image is named by a tag that moves (:1, :latest), and a
		// restart is promised to come back on the installation's current
		// one. Asking the registry each start costs a digest check, not a
		// download, and keeps a node's cached copy from deciding otherwise.
		ImagePullPolicy: corev1.PullAlways,
		Ports:           []corev1.ContainerPort{{ContainerPort: config.MachinePort, Name: "http"}},
		EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: MachineEnvSecretName(in.ObjectName)},
		}}},
		// No liveness probe: a machine that is busy is not one to restart.
		// The front door exits with the agent, which restarts the container.
		ReadinessProbe: &corev1.Probe{ProbeHandler: health, PeriodSeconds: 5},
		StartupProbe:   &corev1.Probe{ProbeHandler: health, PeriodSeconds: 2, FailureThreshold: 150},
		Resources:      corev1.ResourceRequirements{Requests: requests, Limits: limits},
		VolumeMounts:   []corev1.VolumeMount{{Name: "data", MountPath: config.MachineDataPath}},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: new(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
	spec := corev1.PodSpec{
		Containers: []corev1.Container{container},
		// A machine never talks to the cluster it runs on.
		AutomountServiceAccountToken: new(false),
		EnableServiceLinks:           new(false),
		// Whatever the image says: its own unprivileged user, who owns the
		// disk.
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:        new(true),
			RunAsUser:           &user,
			RunAsGroup:          &user,
			FSGroup:             &user,
			FSGroupChangePolicy: &onMismatch,
			SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Volumes: []corev1.Volume{{
			Name: "data",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: MachineDataClaimName(in.ObjectName),
			}},
		}},
	}
	for _, name := range in.ImagePullSecrets {
		spec.ImagePullSecrets = append(spec.ImagePullSecrets, corev1.LocalObjectReference{Name: name})
	}
	if in.RegistryUsername != "" && in.RegistryPassword != "" {
		spec.ImagePullSecrets = append(spec.ImagePullSecrets, corev1.LocalObjectReference{Name: MachinePullSecretName(in.ObjectName)})
	}
	labels := withVersion(MachineLabels(in.ID, RoleMachine), in.Image)
	annotations := map[string]string{AnnotationImage: in.Image, AnnotationHost: in.Host}
	if in.OwnerLabel != "" {
		annotations[AnnotationMachineOwner] = in.OwnerLabel
	}
	if in.SignInExpires != "" {
		annotations[AnnotationSignInExpires] = in.SignInExpires
	}
	// The pods roll whenever the environment does: a start hands the machine
	// fresh credentials, and a Secret alone would not restart anything.
	podAnnotations := map[string]string{AnnotationConfigHash: machineEnvHash(in.Env)}
	one := int32(1)
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        in.ObjectName,
			Namespace:   in.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			// The disk attaches to one node at a time.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: MachineSelector(in.ID)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: podAnnotations},
				Spec:       spec,
			},
		},
	}, nil
}

// machineHealthPath mirrors runtime.MachineHealthPath; this package cannot
// import the one that defines it.
const machineHealthPath = "/.donkey/healthz"

func machineEnvHash(env map[string]string) string {
	var parts []string
	for _, key := range slices.Sorted(maps.Keys(env)) {
		parts = append(parts, key+"="+env[key])
	}
	return configHash(parts...)
}

func machineService(in MachineInput) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      in.ObjectName,
			Namespace: in.Namespace,
			Labels:    MachineLabels(in.ID, RoleMachine),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: MachineSelector(in.ID),
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       80,
				TargetPort: intstr.FromInt32(config.MachinePort),
			}},
		},
	}
}

func machineRoute(in MachineInput) *corev1.ConfigMap {
	return routeConfigMap(MachineRouteName(in.ObjectName), in.Namespace, MachineLabels(in.ID, RoleMachineRoute),
		config.MachineRoute(in.ID, in.Owner, in.Host, MachineBackendURL(in.ObjectName, in.Namespace)))
}

// MachineTrafficObjects is the machine hostname's certificate and optional
// DNS record, where the installation manages those per hostname.
func MachineTrafficObjects(in MachineInput) []runtime.Object {
	if in.Host == "" {
		return nil
	}
	labels := MachineLabels(in.ID, RoleEdge)
	var objects []runtime.Object
	if in.TLS.UsesCertManager() {
		objects = append(objects, managedCertificate(MachineEdgeName(in.ID), in.SystemNamespace,
			MachineEdgeName(in.ID)+"-tls", []string{in.Host}, labels, in.TLS))
	}
	if in.ExternalDNS.Enabled() {
		objects = append(objects, dnsEndpoint(MachineEdgeName(in.ID)+"-dns", in.SystemNamespace,
			[]string{in.Host}, labels, in.ExternalDNS))
	}
	return objects
}
