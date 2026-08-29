package manifests

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestServerCanSampleAndExportContainerMetrics(t *testing.T) {
	in := systemInput()
	deployment := serverDeployment(in)
	env := map[string]string{}
	for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
		env[variable.Name] = variable.Value
	}
	if got, want := env["DONKEY_TELEMETRY_OTLP_ENDPOINT"], TelemetryOTLPEndpoint(in.Namespace); got != want {
		t.Fatalf("server OTLP endpoint=%q, want %q", got, want)
	}

	allowedMetrics := false
	for _, rule := range serverRoleRules() {
		if len(rule.APIGroups) == 1 && rule.APIGroups[0] == "metrics.k8s.io" &&
			len(rule.Resources) == 1 && rule.Resources[0] == "pods" {
			allowedMetrics = hasString(rule.Verbs, "get") && hasString(rule.Verbs, "list")
		}
	}
	if !allowedMetrics {
		t.Fatal("donkey-server cannot read pods from metrics.k8s.io")
	}

	policy := telemetryNetworkPolicy(in)
	serverLabels := map[string]string{
		LabelInstallation: in.Namespace,
		LabelComponent:    ServerName,
	}
	serverCanWriteOTLP := false
	for _, peer := range policy.Spec.Ingress[0].From {
		if peer.PodSelector != nil && mapsEqual(peer.PodSelector.MatchLabels, serverLabels) {
			serverCanWriteOTLP = true
		}
	}
	if !serverCanWriteOTLP {
		t.Fatal("telemetry NetworkPolicy does not admit donkey-server on the OTLP port")
	}
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func TestTelemetryOwnsOnePersistentDuckDBWriter(t *testing.T) {
	in := systemInput()
	objects := telemetryObjects(in)
	var deployment *appsv1.Deployment
	var claim *corev1.PersistentVolumeClaim
	for _, object := range objects {
		switch value := object.(type) {
		case *appsv1.Deployment:
			deployment = value
		case *corev1.PersistentVolumeClaim:
			claim = value
		}
	}
	if deployment == nil || claim == nil {
		t.Fatalf("telemetry objects need a Deployment and PVC: %+v", objects)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 ||
		deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("DuckDB must have exactly one writer with Recreate upgrades: %+v", deployment.Spec)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if len(container.Args) != 1 || container.Args[0] != "telemetry" || len(container.VolumeMounts) != 1 {
		t.Fatalf("unexpected telemetry container: %+v", container)
	}
	if claim.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("DuckDB claim access = %v", claim.Spec.AccessModes)
	}
}

func TestTelemetryCredentialIsIndependentFromLoginSessions(t *testing.T) {
	in := systemInput()
	original := telemetryDeployment(in).Spec.Template.Annotations["donkey.local/config-hash"]
	in.SessionKey = "rotated-session-key"
	if got := telemetryDeployment(in).Spec.Template.Annotations["donkey.local/config-hash"]; got != original {
		t.Fatalf("session rotation changed telemetry config hash: %s -> %s", original, got)
	}
	in.TelemetryKey = "rotated-telemetry-key"
	if got := telemetryDeployment(in).Spec.Template.Annotations["donkey.local/config-hash"]; got == original {
		t.Fatal("telemetry key rotation did not roll donkey-telemetry")
	}
	secret := systemSecret(in)
	if secret.StringData["session-key"] == secret.StringData["telemetry-key"] {
		t.Fatal("login sessions and telemetry must not share a root credential")
	}
}
