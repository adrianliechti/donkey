package kubernetes

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

func TestDeleteAppGatewayObjectsIsSafeWithoutCRDs(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	r := NewRuntime(&Client{Dynamic: dyn}, Options{SystemNamespace: "donkey-system"})
	if err := r.deleteAppGatewayObjects(context.Background(), "acme", "shop"); err != nil {
		t.Fatalf("a cluster with no app-edge objects should be a no-op: %v", err)
	}
}

func TestDeleteAppGatewayObjectsUsesSystemNamespace(t *testing.T) {
	ctx := context.Background()
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	r := NewRuntime(&Client{Dynamic: dyn}, Options{SystemNamespace: "bank-donkey-system"})
	if err := r.deleteAppGatewayObjects(ctx, "acme", "shop"); err != nil {
		t.Fatal(err)
	}
	actions := dyn.Actions()
	assertDeleteActions(t, actions,
		[]string{"httproutes", "gateways", "certificates", "secrets"},
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
