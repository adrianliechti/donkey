package kubernetes

import (
	"bytes"
	"context"
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// These are the YAML keys and object names written by main, with no vnext
// viewer or public-access fields. Test the current-format upgrade separately
// from the older annotation/JSON migrations.
func TestAccessMigrationFromMainYAML(t *testing.T) {
	var warnings bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&warnings)
	t.Cleanup(func() { log.SetOutput(previous) })
	ctx := context.Background()
	clientset := fake.NewClientset()
	r := NewRuntime(&Client{Clientset: clientset}, Options{
		SystemNamespace: "donkey-system", Placement: manifests.Placement{SharedNamespace: "workloads"},
	})
	space := manifests.SpaceMarker("team", "workloads")
	space.Labels = r.c.expectedObjectIdentity(space.Labels)
	space.Data = map[string]string{"config.yaml": "admins:\n  users: [owner@example.com]\n  roles: [old-owners]\ndevelopers:\n  groups: [builders]\n  roles: [old-builders]\ndomains: [team.example.com]\n"}
	platform := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: manifests.SystemConfigMapName, Namespace: "donkey-system", Labels: r.c.expectedObjectIdentity(manifests.SystemLabels(manifests.ServerName)),
	}, Data: map[string]string{"platform.yaml": "admins:\n  users: [root@example.com]\n  roles: [old-root]\n"}}
	route := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "team-web-route", Namespace: "workloads", Labels: r.c.expectedObjectIdentity(manifests.Labels("team", "web", manifests.RoleRoute)),
	}, Data: map[string]string{
		"route.yaml":  "host: web.team.example.com\npath: /\nbackend: http://team-web.workloads.svc:80\n",
		"access.yaml": "users: [reader@example.com]\ngroups: [builders, readers]\nroles: [old-readers]\n",
	}}
	// Current YAML takes precedence over stale fallback records, even when
	// those old records could no longer be decoded.
	space.Data["config.json"] = `{"admins":{"roles":"stale"}}`
	platform.Data["platform.json"] = `{"admins":{"roles":"stale"}}`
	route.Annotations = map[string]string{manifests.AnnotationAccess: `{"roles":"stale"}`}
	for _, cm := range []*corev1.ConfigMap{space, platform, route} {
		if _, err := clientset.CoreV1().ConfigMaps(cm.Namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "team-web", Namespace: "workloads", Labels: r.c.expectedObjectIdentity(manifests.Labels("team", "web", manifests.RoleWorkload)),
	}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "registry/web:existing"}}}}}}
	if _, err := clientset.AppsV1().Deployments("workloads").Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := r.EnsureSpaceConfigs(ctx); err != nil {
			t.Fatal(err)
		}
		if err := rt.ReconcileAccess(ctx, r, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"old-owners", "old-builders", "old-root", "old-readers"} {
		if strings.Count(warnings.String(), name) != 1 {
			t.Fatalf("missing or repeated migration warning for %s: %s", name, &warnings)
		}
	}
	policy, err := r.SpaceConfig(ctx, "team")
	want := config.SpaceConfig{
		Admins: config.AccessConfig{Users: []string{"owner@example.com"}}, Developers: config.AccessConfig{Groups: []string{"builders"}},
		Viewers: config.AccessConfig{Users: []string{"reader@example.com"}, Groups: []string{"readers"}}, Domains: []string{"team.example.com"},
	}
	if err != nil || !reflect.DeepEqual(policy, want) {
		t.Fatalf("main settings were not migrated: %+v, %v", policy, err)
	}
	saved, err := clientset.CoreV1().ConfigMaps("workloads").Get(ctx, route.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := manifests.ReadRoute(saved)
	if err != nil || !got.Login || got.Access != nil || got.Backend != "http://team-web.workloads.svc:80" {
		t.Fatalf("migrated route = %+v, %v", got, err)
	}
	admin, err := r.PlatformConfig(ctx)
	if err != nil || !reflect.DeepEqual(admin.Admins.Users, []string{"root@example.com"}) {
		t.Fatalf("platform admin changed: %+v, %v", admin, err)
	}
	unchanged, err := clientset.AppsV1().Deployments("workloads").Get(ctx, dep.Name, metav1.GetOptions{})
	if err != nil || !reflect.DeepEqual(unchanged.Spec, dep.Spec) {
		t.Fatalf("migration changed the running workload: %+v, %v", unchanged, err)
	}
}

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
