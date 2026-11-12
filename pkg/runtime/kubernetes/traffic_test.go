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

func TestDeleteAppTrafficUsesSystemNamespace(t *testing.T) {
	ctx := context.Background()
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	r := NewRuntime(&Client{Dynamic: dyn}, Options{SystemNamespace: "bank-donkey-system"})
	if err := r.deleteAppTrafficObjects(ctx, "acme", "shop"); err != nil {
		t.Fatal(err)
	}
	assertDeleteActions(t, dyn.Actions(),
		[]string{"certificates", "dnsendpoints", "secrets"},
		map[string]string{
			manifests.LabelInstallation: "bank-donkey-system",
			manifests.LabelSpace:        "acme",
			manifests.LabelApp:          "shop",
			manifests.LabelRole:         manifests.RoleEdge,
		})
	for _, action := range dyn.Actions() {
		if action.GetVerb() == "delete-collection" && action.GetNamespace() != "bank-donkey-system" {
			t.Errorf("%s deletion used namespace %q", action.GetResource().Resource, action.GetNamespace())
		}
	}
}

func TestReconcileAppTrafficRemovesOnlyOmittedKinds(t *testing.T) {
	ctx := context.Background()
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	r := NewRuntime(&Client{Dynamic: dyn}, Options{SystemNamespace: "bank-donkey-system"})
	in := manifests.Input{
		App: &config.AppConfig{Name: "shop"}, Space: "acme",
		Domain: "shop.acme.apps.example.com",
		TLS:    config.InstallTLS{Mode: "cert-manager", IssuerName: "platform-ca"},
	}
	if err := r.reconcileAppTraffic(ctx, in); err != nil {
		t.Fatal(err)
	}
	assertDeleteActions(t, dyn.Actions(), []string{"dnsendpoints"}, map[string]string{
		manifests.LabelInstallation: "bank-donkey-system",
		manifests.LabelSpace:        "acme",
		manifests.LabelApp:          "shop",
		manifests.LabelRole:         manifests.RoleEdge,
	})
}

func TestReconcileAppTrafficUsesExistingRouteHostname(t *testing.T) {
	const (
		installation = "bank-donkey-system"
		workloads    = "bank-apps"
		space        = "team"
	)
	placement := manifests.Placement{SharedNamespace: workloads}
	owned := func(app, role string) map[string]string {
		values := manifests.Labels(space, app, role)
		values[manifests.LabelInstallation] = installation
		return values
	}
	deployment := func(app string) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: placement.Name(space, app), Namespace: workloads,
			Labels: owned(app, manifests.RoleWorkload),
		}}
	}
	route := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: placement.Name(space, "site"), Namespace: workloads,
		Labels: owned("site", manifests.RoleRoute),
		Annotations: map[string]string{
			manifests.AnnotationBackend: "http://team-site.bank-apps.svc:80",
			manifests.AnnotationHost:    "shop.example.test",
		},
	}}

	dynamic := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	dynamic.PrependReactor("patch", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patch := action.(k8stesting.PatchAction)
		var object map[string]any
		if err := json.Unmarshal(patch.GetPatch(), &object); err != nil {
			return true, nil, err
		}
		return true, &unstructured.Unstructured{Object: object}, nil
	})
	r := NewRuntime(&Client{
		Clientset: fake.NewSimpleClientset(deployment("site"), deployment("worker"), route),
		Dynamic:   dynamic,
	}, Options{
		Placement:       placement,
		SystemNamespace: installation,
		TLS: config.InstallTLS{
			Mode: "cert-manager", IssuerName: "platform-ca",
		},
		ExternalDNS: config.InstallExternalDNS{Targets: []string{"192.0.2.10"}},
	})
	if err := r.ReconcileAppTraffic(context.Background()); err != nil {
		t.Fatal(err)
	}

	var patches []k8stesting.PatchAction
	for _, action := range dynamic.Actions() {
		if patch, ok := action.(k8stesting.PatchAction); ok {
			patches = append(patches, patch)
		}
	}
	if len(patches) != 2 {
		t.Fatalf("applied %d objects, want site Certificate and DNSEndpoint: %v", len(patches), dynamic.Actions())
	}
	if got := patches[0].GetResource().Resource; got != "certificates" {
		t.Errorf("first applied resource = %q, want certificates", got)
	}
	if got := patches[1].GetResource().Resource; got != "dnsendpoints" {
		t.Errorf("second applied resource = %q, want dnsendpoints", got)
	}
	var certificate map[string]any
	if err := json.Unmarshal(patches[0].GetPatch(), &certificate); err != nil {
		t.Fatal(err)
	}
	dnsNames, _, _ := unstructured.NestedStringSlice(certificate, "spec", "dnsNames")
	if len(dnsNames) != 1 || dnsNames[0] != "shop.example.test" {
		t.Errorf("Certificate DNS names = %v, want existing route hostname", dnsNames)
	}

	edgeSelector := func(app string) string {
		return labels.Set(map[string]string{
			manifests.LabelInstallation: installation,
			manifests.LabelSpace:        space,
			manifests.LabelApp:          app,
			manifests.LabelRole:         manifests.RoleEdge,
		}).String()
	}
	// The internal worker has no route, so all traffic objects are retracted.
	assertDeleteActionsWithSelectors(t, dynamic.Actions(), []string{
		"certificates", "dnsendpoints", "secrets",
	}, []string{
		edgeSelector("worker"), edgeSelector("worker"), edgeSelector("worker"),
	})
}
