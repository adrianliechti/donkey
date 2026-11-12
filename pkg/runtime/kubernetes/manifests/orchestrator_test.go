package manifests

import (
	"reflect"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/adrianliechti/donkey/internal/config"
)

func envByName(c corev1.Container) map[string]corev1.EnvVar {
	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	return env
}

// The orchestrator is where builds run, so it is the pod that must never be
// cut off mid-build: one replica, replaced only after it drained, with a
// grace period the kubelet honours for the whole drain.
func TestOrchestratorRunsEveryBuildAndDrainsBeforeItGoes(t *testing.T) {
	in := systemInput()
	in.RegistryCASecret = "donkey-registry-ca"
	in.RegistryDockerConfigSecret = "registry-mirror-auth"
	dep := orchestratorDeployment(in)
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 ||
		dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("plans and deploy locks live in one process — want 1 replica, Recreate: %+v", dep.Spec.Strategy)
	}
	pod := dep.Spec.Template.Spec
	if pod.TerminationGracePeriodSeconds == nil ||
		*pod.TerminationGracePeriodSeconds < int64(config.DeployDrainTimeout.Seconds()) {
		t.Fatalf("the grace period must cover the %s drain, got %v", config.DeployDrainTimeout, pod.TerminationGracePeriodSeconds)
	}
	if pod.ServiceAccountName != OrchestratorName {
		t.Errorf("orchestrator runs as %q, want its own ServiceAccount", pod.ServiceAccountName)
	}
	container := pod.Containers[0]
	if !reflect.DeepEqual(container.Args, []string{"orchestrator"}) || container.Image != in.ServerImage {
		t.Fatalf("orchestrator container = %s %v", container.Image, container.Args)
	}
	if container.ReadinessProbe == nil || container.ReadinessProbe.HTTPGet.Path != "/readyz" ||
		container.ReadinessProbe.HTTPGet.Port.IntVal != orchestratorPort {
		t.Error("readiness must follow the drain on the orchestrator's port")
	}
	if got := container.Resources.Requests[corev1.ResourceEphemeralStorage]; got.String() != "3Gi" {
		t.Errorf("build contexts are unpacked here; ephemeral storage request = %s", got.String())
	}

	env := envByName(container)
	if env["DONKEY_LISTEN"].Value != ":8087" {
		t.Errorf("DONKEY_LISTEN = %q", env["DONKEY_LISTEN"].Value)
	}
	for _, name := range []string{
		"DONKEY_BUILDKIT_ADDR", "DONKEY_REGISTRY_URL", "DONKEY_REGISTRY_CA_FILE", "DONKEY_REGISTRY_CONFIG_DIR",
		"DONKEY_AI_MODEL", "OPENAI_BASE_URL", "DONKEY_TELEMETRY_URL", "DONKEY_PLATFORM_ADMINS",
		"DONKEY_SYSTEM_NAMESPACE", "DONKEY_TLS_MODE", "DONKEY_ADDON_PLUGINS",
	} {
		if _, ok := env[name]; !ok {
			t.Errorf("the build path needs %s", name)
		}
	}
	// Credentials arrive by reference, as they do for the server.
	for _, name := range []string{
		"DONKEY_SESSION_KEY", "DONKEY_TELEMETRY_KEY", "DONKEY_REGISTRY_PASSWORD", "OPENAI_API_KEY",
		"DONKEY_BUILDER_INSTRUCTIONS", "DONKEY_PROXY_SERVICES",
	} {
		if e, ok := env[name]; !ok || e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			t.Errorf("%s must be a Secret reference, got %+v", name, e)
		}
	}
	// It never signs anybody in, so it never holds the OIDC client.
	for _, name := range []string{"DONKEY_OIDC_CLIENT_ID", "DONKEY_OIDC_CLIENT_SECRET", "DONKEY_ORCHESTRATOR_URL"} {
		if _, ok := env[name]; ok {
			t.Errorf("the orchestrator should not get %s", name)
		}
	}

	var mounted []string
	for _, m := range container.VolumeMounts {
		mounted = append(mounted, m.Name)
	}
	if !slices.Contains(mounted, "registry-ca") || !slices.Contains(mounted, "registry-auth") {
		t.Errorf("registry Secrets must be mounted where the builds run: %v", mounted)
	}
	// Its one disk is the data volume (the source history); see data_test.go.
	for _, v := range pod.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName != DataPVCName {
			t.Errorf("the orchestrator mounts only the data volume, found claim %s", v.PersistentVolumeClaim.ClaimName)
		}
	}
}

// The API has nothing long-running left, so it rolls like any stateless
// service: the new pod is ready before the old one stops, which stops fast.
func TestServerRollsWithoutWaitingForDeploys(t *testing.T) {
	in := systemInput()
	dep := serverDeployment(in)
	rolling := dep.Spec.Strategy.RollingUpdate
	if dep.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || rolling == nil ||
		rolling.MaxUnavailable == nil || rolling.MaxUnavailable.IntValue() != 0 ||
		rolling.MaxSurge == nil || rolling.MaxSurge.IntValue() != 1 {
		t.Fatalf("server strategy = %+v", dep.Spec.Strategy)
	}
	if grace := dep.Spec.Template.Spec.TerminationGracePeriodSeconds; grace == nil || *grace > 60 {
		t.Fatalf("server grace period should be short now that builds run elsewhere, got %v", grace)
	}
	env := envByName(dep.Spec.Template.Spec.Containers[0])
	if got := env["DONKEY_ORCHESTRATOR_URL"].Value; got != OrchestratorEndpoint(in.Namespace) {
		t.Errorf("DONKEY_ORCHESTRATOR_URL = %q", got)
	}
	if got := env["DONKEY_ORCHESTRATOR_URL"].Value; got != "http://donkey-orchestrator.donkey-system.svc:80" {
		t.Errorf("orchestrator endpoint = %q", got)
	}

	var service *corev1.Service
	for _, obj := range SystemObjects(in) {
		if s, ok := obj.(*corev1.Service); ok && s.Name == OrchestratorName {
			service = s
		}
	}
	if service == nil || service.Spec.Ports[0].Port != 80 || service.Spec.Ports[0].TargetPort.IntVal != orchestratorPort ||
		!reflect.DeepEqual(service.Spec.Selector, SystemSelector(OrchestratorName)) {
		t.Fatalf("orchestrator Service = %+v", service)
	}
}

func peersOf(rule networkingv1.NetworkPolicyIngressRule) []map[string]string {
	var out []map[string]string
	for _, peer := range rule.From {
		out = append(out, peer.PodSelector.MatchLabels)
	}
	return out
}

// Who reaches whom: only the API reaches the orchestrator, and only the
// orchestrator reaches the privileged build engine — the browser-facing API
// no longer can.
func TestOnlyTheServerReachesTheOrchestratorAndOnlyItBuilds(t *testing.T) {
	in := systemInput()
	server := SystemPodSelector(in.Namespace, ServerName)
	orchestrator := SystemPodSelector(in.Namespace, OrchestratorName)

	policy := orchestratorNetworkPolicy(in)
	if !reflect.DeepEqual(policy.Spec.PodSelector.MatchLabels, SystemSelector(OrchestratorName)) {
		t.Fatalf("policy selects %v", policy.Spec.PodSelector.MatchLabels)
	}
	rule := policy.Spec.Ingress[0]
	if got := peersOf(rule); !reflect.DeepEqual(got, []map[string]string{server}) || rule.Ports[0].Port.IntVal != orchestratorPort {
		t.Fatalf("orchestrator admits %v on %v", got, rule.Ports[0].Port)
	}

	if got := peersOf(buildkitNetworkPolicy(in).Spec.Ingress[0]); !reflect.DeepEqual(got, []map[string]string{orchestrator}) {
		t.Fatalf("BuildKit admits %v, want only the orchestrator", got)
	}

	api := telemetryNetworkPolicy(in).Spec.Ingress[1]
	if got := peersOf(api); !reflect.DeepEqual(got, []map[string]string{server, orchestrator}) {
		t.Fatalf("telemetry query API admits %v — releases are recorded by the orchestrator", got)
	}
}

func TestOrchestratorAccess(t *testing.T) {
	t.Run("namespace per space", func(t *testing.T) {
		for _, namespace := range []string{DefaultSystemNamespace, "team-donkey"} {
			in := systemInput()
			in.Namespace = namespace
			objs := orchestratorAccessObjects(in)
			if len(objs) != 1 {
				t.Fatalf("want one binding, got %d objects", len(objs))
			}
			binding := objs[0].(*rbacv1.ClusterRoleBinding)
			if binding.Name != clusterScopedName(OrchestratorName, namespace) || binding.RoleRef.Name != "cluster-admin" ||
				binding.Subjects[0].Name != OrchestratorName || binding.Subjects[0].Namespace != namespace {
				t.Fatalf("binding = %+v", binding)
			}
		}
	})
	t.Run("one workload namespace", func(t *testing.T) {
		in := systemInput()
		in.WorkloadNamespace = "bank-apps"
		roles := map[string]*rbacv1.Role{}
		for _, obj := range orchestratorAccessObjects(in) {
			switch o := obj.(type) {
			case *rbacv1.Role:
				roles[o.Namespace] = o
			case *rbacv1.RoleBinding:
				if o.Subjects[0].Name != OrchestratorName || o.RoleRef.Name != OrchestratorName {
					t.Errorf("binding %s/%s = %+v", o.Namespace, o.Name, o)
				}
			default:
				t.Errorf("a confined installation renders no %T", obj)
			}
		}
		if len(roles) != 2 || roles[in.Namespace] == nil || roles["bank-apps"] == nil {
			t.Fatalf("want a Role in both namespaces, got %v", roles)
		}
		has := func(role *rbacv1.Role, group, resource string) bool {
			for _, rule := range role.Rules {
				if slices.Contains(rule.APIGroups, group) && slices.Contains(rule.Resources, resource) {
					return true
				}
			}
			return false
		}
		for ns, role := range roles {
			for _, need := range [][2]string{{"apps", "deployments"}, {"", "secrets"}, {"", "pods/log"}, {"networking.k8s.io", "networkpolicies"}} {
				if !has(role, need[0], need[1]) {
					t.Errorf("%s: a deploy needs %s/%s", ns, need[0], need[1])
				}
			}
			if has(role, "", "pods/exec") || has(role, "metrics.k8s.io", "pods") {
				t.Errorf("%s: the file browser and resource sampling stay with the API", ns)
			}
		}
		if !has(roles[in.Namespace], "cert-manager.io", "certificates") || has(roles["bank-apps"], "cert-manager.io", "certificates") {
			t.Error("per-app Certificates live beside the gateway, in the system namespace only")
		}
	})
}

// Rotating the session key changes the credential the server presents to
// the orchestrator, so both must roll on it. The OIDC client is the API's
// alone.
func TestOrchestratorRollsWithTheSessionKey(t *testing.T) {
	in := systemInput()
	original := in.orchestratorConfigHash()
	rotated := in
	rotated.SessionKey = "rotated-session-key"
	if rotated.orchestratorConfigHash() == original || rotated.serverConfigHash() == in.serverConfigHash() {
		t.Fatal("a session-key rotation must roll both the server and the orchestrator")
	}
	oidc := in
	oidc.OIDCClientSecret = "rotated-client-secret"
	if oidc.orchestratorConfigHash() != original {
		t.Fatal("the orchestrator rolled on an OIDC client change it never reads")
	}
}
