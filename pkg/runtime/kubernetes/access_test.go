package kubernetes

import (
	"context"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestReconcileAppRoutesProtectsOwnAndSharedAddresses(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewClientset()
	r := NewRuntime(&Client{Clientset: clientset}, Options{Placement: manifests.Placement{SharedNamespace: "workloads"}})
	for _, tc := range []struct{ name, role, app string }{
		{"team-api-route", manifests.RoleRoute, "api"}, {"team-api-route-web-api", manifests.RolePathRoute, "api"}, {"other-route", manifests.RoleRoute, "other"},
	} {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: "workloads",
			Labels: r.c.expectedObjectIdentity(manifests.Labels("team", tc.app, tc.role)), Annotations: map[string]string{manifests.AnnotationAccess: `{"users":["old@example.com"]}`}},
			Data: manifests.RouteData(config.GatewayRoute{Host: "example.com", Backend: "http://api:8000", Access: &config.AccessConfig{Groups: []string{"readers"}}}),
		}
		if _, err := clientset.CoreV1().ConfigMaps("workloads").Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := r.ReconcileAppRoutes(ctx, "team", "api", true); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"team-api-route", "team-api-route-web-api", "other-route"} {
		cm, err := clientset.CoreV1().ConfigMaps("workloads").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		route, err := manifests.ReadRoute(cm)
		if err != nil {
			t.Fatal(err)
		}
		want := name != "other-route"
		if route.Login != want || (route.Access == nil) != want || route.Backend != "http://api:8000" {
			t.Fatalf("%s: %+v", name, route)
		}
		if want && cm.Annotations[manifests.AnnotationAccess] != "" {
			t.Fatal("legacy annotation survived")
		}
	}
}
