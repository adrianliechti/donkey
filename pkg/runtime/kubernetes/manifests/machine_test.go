package manifests

import (
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/adrianliechti/donkey/internal/config"
)

const testMachine = "m0123456789abcdef"

func machineInput() MachineInput {
	return MachineInput{
		ID:              testMachine,
		Namespace:       "donkey-platform",
		SystemNamespace: "donkey-system",
		ObjectName:      testMachine,
		Image:           "ghcr.io/acme/donkey-machine:1.2.3",
		Host:            testMachine + ".platform.example.com",
		Owner:           "owner-subject",
		OwnerLabel:      "ada@example.com",
		Env:             map[string]string{"DONKEY_TOKEN": "sealed", "OPENAI_API_KEY": "dkm_x"},
		Settings:        config.MachineSettings{Image: "ghcr.io/acme/donkey-machine:1.2.3"},
	}
}

func machineObject[T runtime.Object](t *testing.T, objs []runtime.Object) T {
	t.Helper()
	for _, obj := range objs {
		if typed, ok := obj.(T); ok {
			return typed
		}
	}
	var zero T
	t.Fatalf("no %T rendered", zero)
	return zero
}

// A machine is nobody's app: none of its objects may carry the labels every
// app list and space sweep selects on, or it would show up as one — and be
// deleted with one.
func TestMachineObjectsAreNotAnApp(t *testing.T) {
	objs, err := MachineObjects(machineInput())
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, obj := range objs {
		accessor, err := meta.Accessor(obj)
		if err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, obj.GetObjectKind().GroupVersionKind().Kind)
		labels := accessor.GetLabels()
		for _, key := range []string{LabelSpace, LabelApp, LabelAddon} {
			if labels[key] != "" {
				t.Errorf("%s %s carries %s=%s", kinds[len(kinds)-1], accessor.GetName(), key, labels[key])
			}
		}
		if labels[LabelRole] == RoleSpace || labels[LabelRole] == RoleWorkload || labels[LabelRole] == RoleRoute {
			t.Errorf("%s %s has an app's role %q", kinds[len(kinds)-1], accessor.GetName(), labels[LabelRole])
		}
	}
	// What the pod needs comes before it; the route that makes it reachable
	// comes last.
	want := []string{"Namespace", "NetworkPolicy", "Secret", "PersistentVolumeClaim", "Deployment", "Service", "ConfigMap"}
	if !slices.Equal(kinds, want) {
		t.Fatalf("objects = %v, want %v", kinds, want)
	}
	ns := machineObject[*corev1.Namespace](t, objs)
	if ns.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Errorf("machines namespace enforces %q, want restricted", ns.Labels["pod-security.kubernetes.io/enforce"])
	}
}

// The machine runs whatever its owner asks an agent to run. Its pod meets
// the restricted Pod Security Standard, holds no cluster credential, and
// gets its secrets from a Secret and nowhere else.
func TestMachineDeploymentIsLockedDown(t *testing.T) {
	objs, err := MachineObjects(machineInput())
	if err != nil {
		t.Fatal(err)
	}
	dep := machineObject[*appsv1.Deployment](t, objs)
	pod := dep.Spec.Template.Spec
	if pod.Hostname != "donkey" {
		t.Errorf("hostname = %q — the owner's prompt would show the pod's name", pod.Hostname)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the machine is handed a Kubernetes credential")
	}
	sc := pod.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || *sc.RunAsUser != config.MachineUser || *sc.FSGroup != config.MachineUser {
		t.Errorf("pod security context = %+v", sc)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("no default seccomp profile")
	}
	c := pod.Containers[0]
	if c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("the machine may escalate privileges")
	}
	if drop := c.SecurityContext.Capabilities.Drop; len(drop) != 1 || drop[0] != "ALL" {
		t.Errorf("dropped capabilities = %v, want ALL", drop)
	}
	if len(c.Env) != 0 {
		t.Errorf("credentials in the pod spec: %v", c.Env)
	}
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef.Name != MachineEnvSecretName(testMachine) {
		t.Errorf("envFrom = %+v", c.EnvFrom)
	}
	if c.ImagePullPolicy != corev1.PullAlways {
		t.Errorf("pull policy = %q — a restart would keep a node's stale copy of a moving tag", c.ImagePullPolicy)
	}
	if c.LivenessProbe != nil {
		t.Error("a liveness probe would restart a machine that is merely busy")
	}
	if c.ReadinessProbe.HTTPGet.Path != machineHealthPath || c.ReadinessProbe.HTTPGet.Port.IntValue() != config.MachinePort {
		t.Errorf("readiness probe = %+v", c.ReadinessProbe.HTTPGet)
	}
	if got := c.Resources.Limits.Cpu().String(); got != config.DefaultMachineCPU {
		t.Errorf("cpu limit = %s", got)
	}
	if got := c.Resources.Limits.Memory().String(); got != config.DefaultMachineMemory {
		t.Errorf("memory limit = %s", got)
	}
	// The disk attaches to one node at a time.
	if dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType || *dep.Spec.Replicas != 1 {
		t.Errorf("strategy = %s, replicas = %d", dep.Spec.Strategy.Type, *dep.Spec.Replicas)
	}
	if c.VolumeMounts[0].MountPath != config.MachineDataPath || pod.Volumes[0].PersistentVolumeClaim.ClaimName != MachineDataClaimName(testMachine) {
		t.Errorf("disk: %+v / %+v", c.VolumeMounts, pod.Volumes)
	}
	claim := machineObject[*corev1.PersistentVolumeClaim](t, objs)
	if got := claim.Spec.Resources.Requests.Storage().String(); got != config.DefaultMachineStorage {
		t.Errorf("disk size = %s", got)
	}
	// A start hands the machine fresh credentials; the pods must roll for
	// them, and a Secret alone restarts nothing.
	in := machineInput()
	in.Env["DONKEY_TOKEN"] = "sealed-again"
	again, _ := MachineObjects(in)
	if a, b := dep.Spec.Template.Annotations[AnnotationConfigHash], machineObject[*appsv1.Deployment](t, again).Spec.Template.Annotations[AnnotationConfigHash]; a == "" || a == b {
		t.Errorf("config hash %q did not change with the environment (%q)", a, b)
	}
}

func TestMachineRoute(t *testing.T) {
	objs, err := MachineObjects(machineInput())
	if err != nil {
		t.Fatal(err)
	}
	cm := machineObject[*corev1.ConfigMap](t, objs)
	if cm.Labels[LabelMachine] != testMachine || cm.Labels[LabelRole] != RoleMachineRoute {
		t.Fatalf("route labels = %v", cm.Labels)
	}
	route, err := ReadRoute(cm)
	if err != nil {
		t.Fatal(err)
	}
	if route.Machine != testMachine || route.Owner != "owner-subject" || !route.Login || route.Token {
		t.Errorf("route = %+v", route)
	}
	if route.Backend != "http://"+testMachine+".donkey-platform.svc:80" {
		t.Errorf("backend = %q", route.Backend)
	}
	if route.Analytics != config.AnalyticsOff {
		t.Error("a machine's pages would be counted as an app's visits")
	}
}

// Where the installation was given one namespace, there is none to make, and
// names carry the reserved "platform" so they cannot be an app's.
func TestMachineObjectsInASharedNamespace(t *testing.T) {
	in := machineInput()
	in.Namespace, in.SharedNamespace = "workloads", true
	in.ObjectName = Placement{SharedNamespace: "workloads"}.Name(config.MachineSpace, testMachine)
	objs, err := MachineObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range objs {
		if _, ok := obj.(*corev1.Namespace); ok {
			t.Fatal("rendered a namespace donkey does not own")
		}
	}
	if dep := machineObject[*appsv1.Deployment](t, objs); dep.Name != "platform-"+testMachine || dep.Namespace != "workloads" {
		t.Errorf("deployment = %s/%s", dep.Namespace, dep.Name)
	}
	// The fence selects machines only: it must not constrain the apps it
	// shares the namespace with.
	policy := machineObject[*networkingv1.NetworkPolicy](t, objs)
	if got := policy.Spec.PodSelector.MatchLabels; len(got) != 1 || got[LabelRole] != RoleMachine {
		t.Errorf("fence selects %v", got)
	}
}

// Nothing reaches a machine but the gateway, with the owner's requests, and
// donkey-server, which asks it for its folders. Out, it gets DNS, the API,
// the AI endpoint and the package mirror by name, and the network beyond —
// except the link-local range where a cloud's metadata service hands out
// node credentials.
func TestMachinePolicy(t *testing.T) {
	p := MachinePolicy("donkey-platform", "donkey-system", false)
	if len(p.Spec.PolicyTypes) != 2 {
		t.Fatalf("policy types = %v, want both directions fenced", p.Spec.PolicyTypes)
	}
	if len(p.Spec.Ingress) != 1 || len(p.Spec.Ingress[0].From) != 2 {
		t.Fatalf("ingress = %+v, want the gateway and donkey-server alone", p.Spec.Ingress)
	}
	in := p.Spec.Ingress[0]
	for i, role := range []string{SystemRole(GatewayAppName), SystemRole(ServerName)} {
		if in.From[i].PodSelector.MatchLabels[LabelRole] != role ||
			in.From[i].NamespaceSelector.MatchLabels[LabelNamespaceName] != "donkey-system" {
			t.Errorf("ingress peer %d = %+v, want %s of donkey-system", i, in.From[i], role)
		}
	}
	if len(in.Ports) != 1 || in.Ports[0].Port.IntValue() != config.MachinePort {
		t.Errorf("ingress ports = %+v, want the front door only", in.Ports)
	}
	if len(p.Spec.Egress) != 5 {
		t.Fatalf("got %d egress rules, want DNS, API, AI, the package mirror and the network", len(p.Spec.Egress))
	}
	for i, want := range []struct {
		role string
		port int
	}{{SystemRole(ServerName), serverPort}, {SystemRole(AIAppName), aiPort}, {SystemRole(PackagesAppName), packagesPort}} {
		rule := p.Spec.Egress[i+1]
		if rule.To[0].PodSelector.MatchLabels[LabelRole] != want.role || rule.Ports[0].Port.IntValue() != want.port {
			t.Errorf("egress rule %d = %+v, want %s:%d", i+1, rule, want.role, want.port)
		}
	}
	internet := p.Spec.Egress[4]
	if internet.To[0].IPBlock.CIDR != "0.0.0.0/0" || !slices.Contains(internet.To[0].IPBlock.Except, "169.254.0.0/16") {
		t.Errorf("the network rule leaves the metadata range open: %+v", internet.To[0].IPBlock)
	}

	// An installation that keeps its apps off the internet keeps machines
	// off it too: the control plane, and nothing beyond. The mirror is part
	// of that, so an agent there can still install what it builds with.
	restricted := MachinePolicy("donkey-platform", "donkey-system", true)
	if len(restricted.Spec.Egress) != 4 {
		t.Fatalf("restricted egress has %d rules, want DNS, API, AI and the package mirror", len(restricted.Spec.Egress))
	}
	if mirror := restricted.Spec.Egress[3]; mirror.To[0].PodSelector.MatchLabels[LabelRole] != SystemRole(PackagesAppName) {
		t.Errorf("restricted egress leaves the package mirror out: %+v", mirror)
	}
	for _, rule := range restricted.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil {
				t.Errorf("restricted egress still names an address range: %+v", peer.IPBlock)
			}
		}
	}

	// The fence must only ever select this installation's machines.
	u := &unstructured.Unstructured{}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(p)
	if err != nil {
		t.Fatal(err)
	}
	u.Object = raw
	if err := StampInstallation(u, "donkey-system"); err != nil {
		t.Fatal(err)
	}
	selector, _, _ := unstructured.NestedStringMap(u.Object, "spec", "podSelector", "matchLabels")
	if selector[LabelInstallation] != "donkey-system" || selector[LabelRole] != RoleMachine {
		t.Errorf("stamped selector = %v", selector)
	}
}

// The API, the AI endpoint and the package mirror admit machine pods — a
// machine's CLI, agent and package managers call them — but only where the
// installation has machines, and only pods that are machines in the
// namespace machines live in.
func TestControlPlaneAdmitsMachinesOnlyWhenEnabled(t *testing.T) {
	in := systemInput()
	machines := func(p *networkPolicy) []networkingv1.NetworkPolicyPeer {
		var out []networkingv1.NetworkPolicyPeer
		for _, peer := range p.Spec.Ingress[0].From {
			if peer.PodSelector != nil && peer.PodSelector.MatchLabels[LabelRole] == RoleMachine {
				out = append(out, peer)
			}
		}
		return out
	}
	if got := machines(serverNetworkPolicy(in)); len(got) != 0 {
		t.Fatalf("the API admits machines of an installation that has none: %+v", got)
	}
	if got := machines(aiNetworkPolicy(in)); len(got) != 0 {
		t.Fatalf("the AI endpoint admits machines of an installation that has none: %+v", got)
	}
	if got := machines(packagesNetworkPolicy(in)); len(got) != 0 {
		t.Fatalf("the package mirror admits machines of an installation that has none: %+v", got)
	}

	in.Machines = config.MachineSettings{Image: "machine:1"}
	in.NamespacePrefix = "donkey-"
	for name, policy := range map[string]*networkPolicy{
		"API": serverNetworkPolicy(in), "AI": aiNetworkPolicy(in), "package mirror": packagesNetworkPolicy(in),
	} {
		peers := machines(policy)
		if len(peers) != 1 {
			t.Fatalf("%s: %d machine peers", name, len(peers))
		}
		if got := peers[0].NamespaceSelector.MatchLabels[LabelNamespaceName]; got != "donkey-platform" {
			t.Errorf("%s admits machines from namespace %q", name, got)
		}
		if got := peers[0].PodSelector.MatchLabels[LabelInstallation]; got != in.Namespace {
			t.Errorf("%s admits machines of installation %q", name, got)
		}
	}

	in.WorkloadNamespace = "workloads"
	if got := machines(serverNetworkPolicy(in))[0].NamespaceSelector.MatchLabels[LabelNamespaceName]; got != "workloads" {
		t.Errorf("shared placement: machines admitted from %q", got)
	}
}

// Where certificates and DNS are managed per hostname, a machine's address
// gets both — beside the gateway, like an app's, and labelled as the
// machine's so they go when it stops.
func TestMachineTrafficObjects(t *testing.T) {
	in := machineInput()
	if objs := MachineTrafficObjects(in); len(objs) != 0 {
		t.Fatalf("rendered %d traffic objects without cert-manager or external-dns", len(objs))
	}
	in.TLS = config.InstallTLS{Mode: "cert-manager", IssuerName: "issuer"}
	in.ExternalDNS = config.InstallExternalDNS{Targets: []string{"203.0.113.10"}}
	objs := MachineTrafficObjects(in)
	if len(objs) != 2 {
		t.Fatalf("rendered %d traffic objects, want a Certificate and a DNSEndpoint", len(objs))
	}
	for _, obj := range objs {
		u := obj.(*unstructured.Unstructured)
		if u.GetNamespace() != "donkey-system" {
			t.Errorf("%s lives in %s, want the gateway's namespace", u.GetKind(), u.GetNamespace())
		}
		if labels := u.GetLabels(); labels[LabelMachine] != testMachine || labels[LabelRole] != RoleEdge || labels[LabelSpace] != "" {
			t.Errorf("%s labels = %v", u.GetKind(), labels)
		}
	}
	names, _, _ := unstructured.NestedStringSlice(objs[0].(*unstructured.Unstructured).Object, "spec", "dnsNames")
	if len(names) != 1 || names[0] != in.Host {
		t.Errorf("certificate names = %v", names)
	}
}
