package manifests

import (
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

func TestServiceProxyIsSharedReadOnlyAndReloadsSecret(t *testing.T) {
	in := systemInput()
	in.WorkloadNamespace = "workloads"
	in.ProxyServices = config.ProxyServices{{Name: "erp", TargetURL: "https://user:private-password@enterprise.local/api", ProxyURL: "https://proxy.local:8443"}}
	dep := proxyDeployment(in)
	container := dep.Spec.Template.Spec.Containers[0]
	if *dep.Spec.Replicas != 1 || container.Args[0] != "proxy" || dep.Spec.Template.Spec.ServiceAccountName != ProxyAppName {
		t.Fatal("proxy must be one shared workload with its own identity")
	}
	for _, env := range container.Env {
		if env.Name == "DONKEY_SESSION_KEY" || env.Name == "DONKEY_REGISTRY_PASSWORD" || env.Name == "OPENAI_API_KEY" {
			t.Errorf("proxy inherited an unrelated credential: %s", env.Name)
		}
	}
	if len(container.VolumeMounts) != 1 || !container.VolumeMounts[0].ReadOnly || container.VolumeMounts[0].SubPath != "" {
		t.Fatal("configuration must be a projected directory, so Secret updates become visible")
	}
	var foundSecret, foundRole bool
	for _, obj := range proxyObjects(in) {
		switch v := obj.(type) {
		case *corev1.Secret:
			foundSecret = string(v.Data[ProxyConfigKey]) == in.ProxyServices.JSON()
		case *rbacv1.Role:
			foundRole = v.Namespace == "workloads"
			for _, rule := range v.Rules {
				if len(rule.Resources) != 2 || rule.Resources[0] != "secrets" || rule.Resources[1] != "pods" {
					t.Fatal("proxy role must read only bindings and pod identities")
				}
				for _, verb := range rule.Verbs {
					if verb != "get" && verb != "list" && verb != "watch" {
						t.Fatalf("proxy has write verb %q", verb)
					}
				}
			}
		case *rbacv1.ClusterRole:
			t.Fatal("shared namespace placement must not grant cluster-wide access")
		}
	}
	if !foundSecret || !foundRole {
		t.Fatal("proxy configuration or binding role was omitted")
	}
	in.ProxyServices[0].TargetURL = "https://changed.enterprise.local/other"
	if dep.Spec.Template.Annotations[AnnotationConfigHash] != proxyDeployment(in).Spec.Template.Annotations[AnnotationConfigHash] {
		t.Fatal("target changes must not restart the proxy")
	}
}
