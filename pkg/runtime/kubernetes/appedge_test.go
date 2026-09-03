package kubernetes

import (
	"context"
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

func TestDeleteAppEdgeObjectsIsSafeWithoutCRDs(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	r := NewRuntime(&Client{Dynamic: dyn}, Options{SystemNamespace: "donkey-system"})
	if err := r.deleteAppEdgeObjects(context.Background(), "acme", "shop"); err != nil {
		t.Fatalf("a cluster with no app-edge objects should be a no-op: %v", err)
	}
}

func TestDeleteAppEdgeObjectsUsesSystemNamespace(t *testing.T) {
	ctx := context.Background()
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	r := NewRuntime(&Client{Dynamic: dyn}, Options{SystemNamespace: "bank-donkey-system"})
	if err := r.deleteAppEdgeObjects(ctx, "acme", "shop"); err != nil {
		t.Fatal(err)
	}
	actions := dyn.Actions()
	assertDeleteActions(t, actions,
		[]string{"httproutes", "gateways", "httpproxies", "certificates", "secrets"},
		map[string]string{
			manifests.LabelInstallation: "bank-donkey-system",
			manifests.LabelSpace:        "acme",
			manifests.LabelApp:          "shop",
			manifests.LabelRole:         manifests.RoleEdge,
		})
	for _, action := range actions {
		if action.GetVerb() == "delete-collection" && action.GetNamespace() != "bank-donkey-system" {
			t.Errorf("%s deletion used namespace %q", action.GetResource().Resource, action.GetNamespace())
		}
	}
}

func TestReconcileAppEdgeModeRemovesOnlyInactiveStrategy(t *testing.T) {
	ctx := context.Background()
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	r := NewRuntime(&Client{Dynamic: dyn}, Options{SystemNamespace: "bank-donkey-system"})
	in := manifests.Input{
		App:    &config.AppConfig{Name: "shop"},
		Space:  "acme",
		Domain: "shop.acme.apps.example.com",
		ContourHTTPProxy: manifests.ContourHTTPProxyConfig{
			Namespace:        "bank-donkey-system",
			IngressClassName: "contour",
			IssuerName:       "platform-ca",
		},
	}
	if err := r.reconcileAppEdgeMode(ctx, in); err != nil {
		t.Fatal(err)
	}
	assertDeleteActions(t, dyn.Actions(), []string{"httproutes", "gateways"}, map[string]string{
		manifests.LabelInstallation: "bank-donkey-system",
		manifests.LabelSpace:        "acme",
		manifests.LabelApp:          "shop",
		manifests.LabelRole:         manifests.RoleEdge,
	})

	dyn.ClearActions()
	in.ContourHTTPProxy = manifests.ContourHTTPProxyConfig{}
	in.AppGateway = manifests.AppGatewayConfig{Namespace: "bank-donkey-system", ClassName: "platform", IssuerName: "platform-ca"}
	if err := r.reconcileAppEdgeMode(ctx, in); err != nil {
		t.Fatal(err)
	}
	assertDeleteActions(t, dyn.Actions(), []string{"httpproxies"}, map[string]string{
		manifests.LabelInstallation: "bank-donkey-system",
		manifests.LabelSpace:        "acme",
		manifests.LabelApp:          "shop",
		manifests.LabelRole:         manifests.RoleEdge,
	})
}

func TestReconcileAppEdgesSwitchesEveryExistingAppFromActualRouteState(t *testing.T) {
	const (
		installation = "bank-donkey-system"
		workloads    = "bank-apps"
		space        = "team"
	)
	placement := manifests.Placement{SharedNamespace: workloads}
	owned := func(app, role string) map[string]string {
		labels := manifests.Labels(space, app, role)
		labels[manifests.LabelInstallation] = installation
		return labels
	}
	deployment := func(app string) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: placement.Name(space, app), Namespace: workloads,
			Labels: owned(app, manifests.RoleWorkload),
		}}
	}
	siteRoute := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: placement.Name(space, "site"), Namespace: workloads,
		Labels: owned("site", manifests.RoleRoute),
		Annotations: map[string]string{
			manifests.AnnotationBackend: "http://team--site.bank-apps.svc:80",
			manifests.AnnotationHost:    "shop.example.test",
		},
	}}

	dynamic := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	// The dynamic fake records apply patches but does not create an object for
	// an apply-on-missing request. Return the submitted object so the test can
	// exercise Client.Apply's real action path.
	dynamic.PrependReactor("patch", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patch := action.(k8stesting.PatchAction)
		var object map[string]any
		if err := json.Unmarshal(patch.GetPatch(), &object); err != nil {
			return true, nil, err
		}
		return true, &unstructured.Unstructured{Object: object}, nil
	})
	r := NewRuntime(&Client{
		Clientset: fake.NewSimpleClientset(deployment("site"), deployment("worker"), siteRoute),
		Dynamic:   dynamic,
	}, Options{
		Placement:       placement,
		SystemNamespace: installation,
		ContourHTTPProxy: config.InstallContourHTTPProxy{
			IngressClassName: "contour-main",
			IssuerName:       "platform-ca",
		},
	})
	if err := r.ReconcileAppEdges(context.Background()); err != nil {
		t.Fatal(err)
	}

	var patches []k8stesting.PatchAction
	for _, action := range dynamic.Actions() {
		if patch, ok := action.(k8stesting.PatchAction); ok {
			patches = append(patches, patch)
		}
	}
	if len(patches) != 2 {
		t.Fatalf("applied %d objects, want site Certificate and HTTPProxy: %v", len(patches), dynamic.Actions())
	}
	if got := patches[0].GetResource().Resource; got != "certificates" {
		t.Errorf("first applied resource = %q, want certificates", got)
	}
	if got := patches[1].GetResource().Resource; got != "httpproxies" {
		t.Errorf("second applied resource = %q, want httpproxies", got)
	}
	var proxy map[string]any
	if err := json.Unmarshal(patches[1].GetPatch(), &proxy); err != nil {
		t.Fatal(err)
	}
	if host, _, _ := unstructured.NestedString(proxy, "spec", "virtualhost", "fqdn"); host != "shop.example.test" {
		t.Errorf("HTTPProxy host = %q, want the existing route's exact hostname", host)
	}

	edgeSelector := func(app string) string {
		return labels.Set(map[string]string{
			manifests.LabelInstallation: installation,
			manifests.LabelSpace:        space,
			manifests.LabelApp:          app,
			manifests.LabelRole:         manifests.RoleEdge,
		}).String()
	}
	assertDeleteActionsWithSelectors(t, dynamic.Actions(), []string{
		"httproutes", "gateways",
		"httproutes", "gateways", "httpproxies", "certificates", "secrets",
	}, []string{
		edgeSelector("site"), edgeSelector("site"),
		edgeSelector("worker"), edgeSelector("worker"), edgeSelector("worker"),
		edgeSelector("worker"), edgeSelector("worker"),
	})
}
