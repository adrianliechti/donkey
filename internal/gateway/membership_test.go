package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime/docker"
	k8s "github.com/adrianliechti/donkey/pkg/runtime/kubernetes"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type unavailablePolicySource struct{}

func (unavailablePolicySource) list(context.Context) ([]route, error) {
	return nil, errors.New("policy store unavailable")
}
func TestPolicyReadFailureRevokesCachedAppRoutes(t *testing.T) {
	s := testServer()
	s.source = unavailablePolicySource{}
	s.routes = map[string][]route{"app.example.com": {{Space: "team", App: "web"}}, "console.example.com": {{Host: "console.example.com"}}}
	if err := s.refreshRoutes(context.Background()); err == nil {
		t.Fatal("missing policy error")
	}
	if len(s.routes["app.example.com"]) != 0 || len(s.routes["console.example.com"]) != 1 {
		t.Fatalf("unsafe cached routes: %+v", s.routes)
	}
}

func TestGatewayUsesCurrentSpacePolicy(t *testing.T) {
	ctx := context.Background()
	cm := manifests.SpaceMarker("team", "workloads")
	cm.Labels[manifests.LabelInstallation] = "donkey-system"
	cm.Data = map[string]string{"config.yaml": "viewers:\n  groups: [readers]\nadmins:\n  users: [owner@example.com]\ndevelopers:\n  users: [dev@example.com]\n"}
	routeCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "web-route", Namespace: "workloads", Labels: manifests.Labels("team", "web", manifests.RoleRoute),
	}, Data: manifests.RouteData(config.GatewayRoute{Host: "web.example.com", Path: "/", Backend: "http://web:8000"})}
	routeCM.Labels[manifests.LabelInstallation] = "donkey-system"
	platformCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: manifests.SystemConfigMapName, Namespace: "donkey-system", Labels: manifests.SystemLabels(manifests.ServerName),
	}, Data: map[string]string{"platform.yaml": "admins:\n  users: [root@example.com]\n"}}
	platformCM.Labels[manifests.LabelInstallation] = "donkey-system"
	clientset := fake.NewClientset(cm, routeCM, platformCM)
	client := (&k8s.Client{Clientset: clientset}).ForInstallation("donkey-system", "workloads")
	s := testServer()
	s.source = k8sRoutes{client: client}
	if err := s.refreshRoutes(ctx); err != nil {
		t.Fatal(err)
	}
	rt := s.routes["web.example.com"][0]
	if !rt.Login || !rt.allows("u", "viewer@example.com", []string{"readers"}) {
		t.Fatalf("default policy not enforced: %+v", rt)
	}
	for _, email := range []string{"owner@example.com", "dev@example.com", "root@example.com"} {
		if !rt.allows("u", email, nil) {
			t.Errorf("member %s denied", email)
		}
	}
	if rt.allows("u", "stranger@example.com", nil) {
		t.Fatal("unassigned sign-in admitted")
	}
	cm.Data["config.yaml"] = "allowPublic: true\n"
	if _, err := clientset.CoreV1().ConfigMaps(cm.Namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.refreshRoutes(ctx); err != nil {
		t.Fatal(err)
	}
	rt = s.routes["web.example.com"][0]
	if rt.Login || rt.allows("u", "viewer@example.com", []string{"readers"}) {
		t.Fatal("membership removal or public opt-in was not observed")
	}
	cm.Data["config.yaml"] = "allowPublic: false\n"
	if _, err := clientset.CoreV1().ConfigMaps(cm.Namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.refreshRoutes(ctx); err != nil {
		t.Fatal(err)
	}
	if !s.routes["web.example.com"][0].Login {
		t.Fatal("stale public route bypassed the space policy")
	}
}

func TestFileGatewayUsesSpaceMembershipAndDefaultPublicPolicy(t *testing.T) {
	dir := t.TempDir()
	if err := docker.WriteSystemRoute(dir, "web", docker.Route{Space: "team", App: "web", Host: "web.example.com", Backend: "http://web:8000"}); err != nil {
		t.Fatal(err)
	}
	write := func(value string) {
		t.Helper()
		path := filepath.Join(dir, "spaces", "team")
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "config.json"), []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	source := fileRoutes{dir: dir}
	write(`{"viewers":{"users":["viewer@example.com"]}}`)
	routes, err := source.list(context.Background())
	if err != nil || len(routes) != 1 || !routes[0].Login || !routes[0].allows("u", "viewer@example.com", nil) {
		t.Fatalf("routes=%+v err=%v", routes, err)
	}
	write(`{"allowPublic":true,"viewers":{}}`)
	routes, err = source.list(context.Background())
	if err != nil || routes[0].Login || routes[0].allows("u", "viewer@example.com", nil) {
		t.Fatalf("stale file policy: %+v %v", routes, err)
	}
}
