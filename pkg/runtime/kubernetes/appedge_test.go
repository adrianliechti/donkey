package kubernetes

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

func TestDeleteAppGatewayObjectsIsSafeWithoutCRDs(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	r := NewRuntime(&Client{Dynamic: dyn}, Options{SystemNamespace: "donkey-system"})
	if err := r.deleteAppGatewayObjects(context.Background(), "acme--shop"); err != nil {
		t.Fatalf("a cluster with no app-edge objects should be a no-op: %v", err)
	}
}

func TestDeleteAppGatewayObjectsUsesSystemNamespace(t *testing.T) {
	ctx := context.Background()
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	for _, object := range []struct {
		kind string
		name string
	}{
		{kind: "Certificate", name: manifests.AppEdgeName("acme--shop")},
		{kind: "Gateway", name: manifests.AppGatewayName("bank-donkey-system", "acme--shop")},
		{kind: "HTTPRoute", name: manifests.AppEdgeName("acme--shop")},
		{kind: "Secret", name: manifests.AppCertificateSecretName("acme--shop")},
	} {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": gvrFor[object.kind].GroupVersion().String(),
			"kind":       object.kind,
			"metadata": map[string]any{
				"name":      object.name,
				"namespace": "bank-donkey-system",
			},
		}}
		if _, err := dyn.Resource(gvrFor[object.kind]).Namespace("bank-donkey-system").Create(ctx, u, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	r := NewRuntime(&Client{Dynamic: dyn}, Options{SystemNamespace: "bank-donkey-system"})
	if err := r.deleteAppGatewayObjects(ctx, "acme--shop"); err != nil {
		t.Fatal(err)
	}
	for _, object := range []struct {
		kind string
		name string
	}{
		{kind: "Certificate", name: manifests.AppEdgeName("acme--shop")},
		{kind: "Gateway", name: manifests.AppGatewayName("bank-donkey-system", "acme--shop")},
		{kind: "HTTPRoute", name: manifests.AppEdgeName("acme--shop")},
		{kind: "Secret", name: manifests.AppCertificateSecretName("acme--shop")},
	} {
		_, err := dyn.Resource(gvrFor[object.kind]).Namespace("bank-donkey-system").Get(ctx, object.name, metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			t.Errorf("%s was left behind (err=%v)", object.kind, err)
		}
	}
}
