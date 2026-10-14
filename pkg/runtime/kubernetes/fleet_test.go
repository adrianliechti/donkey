package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func fleetTestRuntime(t *testing.T, shared string) (*Runtime, *fake.Clientset) {
	t.Helper()
	placement := manifests.Placement{SharedNamespace: shared, NamespacePrefix: "donkey-"}
	ns, name := placement.Namespace("team"), placement.Name("team", "api")
	stamp := func(values map[string]string) map[string]string {
		values[manifests.LabelInstallation] = manifests.DefaultSystemNamespace
		return values
	}
	workload := stamp(manifests.Labels("team", "api", manifests.RoleWorkload))
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: workload, Generation: 2},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: workload},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: manifests.WebContainerName, Image: "example/api:v1", Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
			}}}},
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-pod", Namespace: ns, Labels: workload,
			CreationTimestamp: metav1.NewTime(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: manifests.WebContainerName, Ready: true, RestartCount: 3,
		}}},
	}
	route := manifests.AppRoute(manifests.Input{
		App: &config.AppConfig{Name: "api"}, Space: "team", Namespace: ns, ObjectName: name, Domain: "api.team.example.com",
	}).(*corev1.ConfigMap)
	route.Labels = stamp(route.Labels)
	// Exercise the legacy route name and preserved PVC name as well as the
	// current Deployment: both continue to contribute to fleet display values.
	route.Name = name
	claimName := manifests.DataClaimName(name)
	if shared != "" {
		claimName = "team--api-data"
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: claimName, Namespace: ns, Labels: stamp(manifests.Labels("team", "api", manifests.RoleStorage)),
	}}
	bindingLabels := stamp(manifests.AddonLabels("team", "db", manifests.RoleBinding))
	bindingLabels[manifests.LabelApp] = "api"
	binding := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: name + "-db", Namespace: ns, Labels: bindingLabels,
	}, Data: map[string][]byte{"DATABASE_URL": []byte("postgres://db")}}
	marker := manifests.SpaceMarker("team", ns)
	marker.Labels = stamp(marker.Labels)
	marker.Data = map[string]string{spaceConfigKey: "developers:\n  users: [dev@example.com]\n"}
	objects := []k8sruntime.Object{dep, pod, route, pvc, binding, marker}
	if shared == "" {
		namespace := manifests.Namespace("team", ns)
		namespace.Labels = stamp(namespace.Labels)
		objects = append(objects, namespace)
	}
	cs := fake.NewClientset(objects...)
	r := NewRuntime(&Client{Clientset: cs}, Options{Placement: placement})
	return r, cs
}

func TestBulkFleetMatchesIndividualReads(t *testing.T) {
	for _, shared := range []string{"", "workloads"} {
		t.Run("shared="+shared, func(t *testing.T) {
			r, _ := fleetTestRuntime(t, shared)
			apps, err := r.AppSummaries(context.Background())
			if err != nil || len(apps) != 1 {
				t.Fatalf("fleet = %+v, err = %v", apps, err)
			}
			st, err := r.Status(context.Background(), "team", "api")
			if err != nil || !reflect.DeepEqual(apps[0].Status, *st) {
				t.Fatalf("bulk status %+v differs from individual %+v: %v", apps[0].Status, st, err)
			}
			if !st.Healthy() || st.Restarts != 3 || st.PublicHost != "api.team.example.com" {
				t.Fatalf("incomplete status: %+v", st)
			}
			if !apps[0].HasStorage || !r.HasData(context.Background(), "team", "api") ||
				!reflect.DeepEqual(apps[0].Addons, []string{"db"}) {
				t.Fatalf("missing storage or service: %+v", apps[0])
			}
		})
	}
}

func TestFleetCacheCopiesExpiresAndRecoversFromAPIFailure(t *testing.T) {
	r, cs := fleetTestRuntime(t, "")
	ctx := context.Background()
	apps, err := r.AppSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	apps[0].Status.Image = "caller mutation"
	apps[0].Status.Instances[0] = "caller mutation"
	apps[0].Addons[0] = "caller mutation"
	cs.ClearActions()
	apps, err = r.AppSummaries(ctx)
	if err != nil || apps[0].Status.Image != "example/api:v1" || apps[0].Addons[0] != "db" ||
		apps[0].Status.Instances[0] == "caller mutation" || len(cs.Actions()) != 0 {
		t.Fatalf("cached snapshot was mutated or fetched again: %+v %v", apps, err)
	}
	dep, _ := cs.AppsV1().Deployments(r.ns("team")).Get(ctx, r.name("team", "api"), metav1.GetOptions{})
	dep.Spec.Template.Spec.Containers[0].Image = "example/api:v2"
	if _, err := cs.AppsV1().Deployments(dep.Namespace).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Avoid wall-clock sleeps: an expired cache must observe external changes.
	r.fleet.mu.Lock()
	r.fleet.until = time.Now().Add(-time.Second)
	r.fleet.mu.Unlock()
	apps, err = r.AppSummaries(ctx)
	if err != nil || apps[0].Status.Image != "example/api:v2" {
		t.Fatalf("expiry did not refresh workload data: %+v %v", apps, err)
	}
	fail := true
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		if fail {
			return true, nil, errors.New("API unavailable")
		}
		return false, nil, nil
	})
	r.InvalidateAppSummaries()
	cs.ClearActions()
	apps, err = r.AppSummaries(ctx)
	if err == nil || apps != nil {
		t.Fatalf("failed refresh served stale status: %+v %v", apps, err)
	}
	reads := len(cs.Actions())
	if _, err := r.AppSummaries(ctx); err == nil || len(cs.Actions()) != reads {
		t.Fatal("API failure caused repeated sweeps for concurrent retries")
	}
	fail = false
	r.InvalidateAppSummaries()
	if apps, err := r.AppSummaries(ctx); err != nil || len(apps) != 1 {
		t.Fatalf("cache did not recover: %+v %v", apps, err)
	}
	cs.ClearActions()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.AppSummaries(canceled); !errors.Is(err, context.Canceled) || len(cs.Actions()) != 0 {
		t.Fatal("canceled request read fleet data")
	}
	inFlightCtx, cancelInFlight := context.WithCancel(ctx)
	cancelDuringRead := true
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		if cancelDuringRead {
			cancelInFlight()
			return true, nil, inFlightCtx.Err()
		}
		return false, nil, nil
	})
	r.InvalidateAppSummaries()
	if _, err := r.AppSummaries(inFlightCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight canceled read err = %v", err)
	}
	cancelDuringRead = false
	if apps, err := r.AppSummaries(ctx); err != nil || len(apps) != 1 {
		t.Fatalf("caller cancellation poisoned shared fleet cache: %+v %v", apps, err)
	}
}

func TestFleetFiltersOwnershipAndConfinesReads(t *testing.T) {
	r, cs := fleetTestRuntime(t, "workloads")
	ctx := context.Background()
	dep, _ := cs.AppsV1().Deployments("workloads").Get(ctx, "team-api", metav1.GetOptions{})
	foreign := dep.DeepCopy()
	foreign.Name = "foreign"
	foreign.Labels[manifests.LabelInstallation] = "another-installation"
	if _, err := cs.AppsV1().Deployments("workloads").Create(ctx, foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	collision := dep.DeepCopy()
	collision.Name = "addon-with-workload-role"
	collision.Labels[manifests.LabelAddon] = "db"
	if _, err := cs.AppsV1().Deployments("workloads").Create(ctx, collision, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	nonCanonical := dep.DeepCopy()
	nonCanonical.Name = "team--api"
	if _, err := cs.AppsV1().Deployments("workloads").Create(ctx, nonCanonical, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	moved := dep.DeepCopy()
	moved.Name, moved.Namespace = "wrong-home", manifests.DefaultSystemNamespace
	if _, err := cs.AppsV1().Deployments(moved.Namespace).Create(ctx, moved, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	cs.ClearActions()
	apps, err := r.AppSummaries(ctx)
	if err != nil || len(apps) != 1 {
		t.Fatalf("fleet included objects outside its app identity: %+v %v", apps, err)
	}
	settings, err := r.ListSpaceConfigs(ctx)
	if err != nil || len(settings) != 1 || settings["team"].Developers.Users[0] != "dev@example.com" {
		t.Fatalf("bulk settings: %+v %v", settings, err)
	}
	for _, action := range cs.Actions() {
		if action.GetVerb() != "list" || action.GetNamespace() != "workloads" {
			t.Fatalf("confined fleet made a non-workload or non-list read: %s %s %s", action.GetVerb(), action.GetResource().Resource, action.GetNamespace())
		}
		selector := action.(k8stesting.ListAction).GetListRestrictions().Labels
		if !strings.Contains(selector.String(), manifests.LabelInstallation+"="+r.c.installation()) {
			t.Fatalf("unscoped list: %s", selector)
		}
	}
}

func TestBulkSpacePoliciesFailClosed(t *testing.T) {
	for _, shared := range []string{"", "workloads"} {
		t.Run(fmt.Sprintf("shared=%s", shared), func(t *testing.T) {
			r, cs := fleetTestRuntime(t, shared)
			ctx := context.Background()
			cm, _ := cs.CoreV1().ConfigMaps(r.ns("team")).Get(ctx, manifests.SpaceMarkerName("team"), metav1.GetOptions{})
			cm.Data[spaceConfigKey] = "developers: corrupt"
			if _, err := cs.CoreV1().ConfigMaps(cm.Namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if settings, err := r.ListSpaceConfigs(ctx); err == nil || settings != nil {
				t.Fatalf("corrupt policy granted access: %+v %v", settings, err)
			}
			cm.Data[spaceConfigKey] = "developers:\n  users: [dev@example.com]\n"
			cm.Labels[manifests.LabelInstallation] = "another-installation"
			if _, err := cs.CoreV1().ConfigMaps(cm.Namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			settings, err := r.ListSpaceConfigs(ctx)
			if shared == "" && err == nil {
				t.Fatal("owned namespace accepted a foreign space policy")
			}
			if shared != "" && (err != nil || len(settings) != 0) {
				t.Fatalf("confined installation did not exclude foreign policies: %+v %v", settings, err)
			}
		})
	}
}
