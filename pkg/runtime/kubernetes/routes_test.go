package kubernetes

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// Routes belong to an installation, not to one deployment tool. A Helm or
// GitOps-managed ConfigMap must be discoverable without pretending that
// donkey owns and may delete the object.
func TestListManagedRoutesAcceptsExternalManagers(t *testing.T) {
	const (
		instance  = "bank-donkey-system"
		namespace = "bank-donkey-apps"
	)
	route := func(name, owner, manager, backend string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				manifests.LabelInstance:          owner,
				"app.kubernetes.io/managed-by": manager,
			},
			Annotations: map[string]string{manifests.AnnotationBackend: backend},
		}}
	}
	objects := []runtime.Object{
		route("helm", instance, "Helm", "http://site.bank-donkey-apps.svc:80"),
		route("donkey", instance, manifests.ManagedByValue, "http://api.bank-donkey-apps.svc:80"),
		route("foreign", "other-donkey-system", "Helm", "http://foreign.other.svc:80"),
		route("unstamped", "", "Helm", "http://unstamped.other.svc:80"),
		route("not-a-route", instance, "Helm", ""),
	}

	client := (&Client{Clientset: fake.NewSimpleClientset(objects...)}).
		ForInstallation(instance, namespace)
	routes, err := client.ListManagedRoutes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 {
		t.Fatalf("got %d routes, want the Helm and donkey routes: %+v", len(routes), routes)
	}
	seen := map[string]bool{}
	for _, route := range routes {
		seen[route.Name] = true
	}
	for _, want := range []string{"helm", "donkey"} {
		if !seen[want] {
			t.Errorf("route %q was not discovered", want)
		}
	}
}
