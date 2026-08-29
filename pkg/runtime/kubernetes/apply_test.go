package kubernetes

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

func TestStampInstallationCoversWorkloadTemplatesAndSelectors(t *testing.T) {
	client := &Client{Installation: "donkey-test"}
	tests := []struct {
		kind       string
		apiVersion string
		object     map[string]any
		paths      [][]string
	}{
		{
			kind: "Deployment", apiVersion: "apps/v1",
			object: map[string]any{"spec": map[string]any{
				"selector": map[string]any{"matchLabels": map[string]any{"existing": "selector"}},
				"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"existing": "pod"}}},
			}},
			paths: [][]string{{"spec", "selector", "matchLabels"}, {"spec", "template", "metadata", "labels"}},
		},
		{
			kind: "Job", apiVersion: "batch/v1",
			object: map[string]any{"spec": map[string]any{
				"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"existing": "pod"}}},
			}},
			paths: [][]string{{"spec", "template", "metadata", "labels"}},
		},
		{
			kind: "Service", apiVersion: "v1",
			object: map[string]any{"spec": map[string]any{
				"selector": map[string]any{"existing": "selector"},
			}},
			paths: [][]string{{"spec", "selector"}},
		},
		{
			kind: "NetworkPolicy", apiVersion: "networking.k8s.io/v1",
			object: map[string]any{"spec": map[string]any{
				"podSelector": map[string]any{"matchLabels": map[string]any{"existing": "selector"}},
			}},
			paths: [][]string{{"spec", "podSelector", "matchLabels"}},
		},
		{
			kind: "Certificate", apiVersion: "cert-manager.io/v1",
			object: map[string]any{"spec": map[string]any{
				"secretTemplate": map[string]any{"labels": map[string]any{"existing": "secret"}},
			}},
			paths: [][]string{{"spec", "secretTemplate", "labels"}},
		},
	}
	for _, test := range tests {
		t.Run(test.kind, func(t *testing.T) {
			object := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": test.apiVersion,
				"kind":       test.kind,
				"metadata": map[string]any{
					"name":   "demo",
					"labels": map[string]any{"existing": "object"},
				},
			}}
			for key, value := range test.object {
				object.Object[key] = value
			}
			if err := client.stampInstallation(object); err != nil {
				t.Fatal(err)
			}
			if got := object.GetLabels()[manifests.LabelInstallation]; got != client.Installation {
				t.Errorf("object installation = %q", got)
			}
			for _, path := range test.paths {
				values, found, err := unstructured.NestedStringMap(object.Object, path...)
				if err != nil || !found {
					t.Fatalf("%v: found=%v err=%v", path, found, err)
				}
				if got := values[manifests.LabelInstallation]; got != client.Installation {
					t.Errorf("%v installation = %q", path, got)
				}
			}
		})
	}
}

func TestStampInstallationLeavesSelectorlessServiceSelectorless(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": "external"},
		"spec":     map[string]any{"type": "ExternalName", "externalName": "example.com"},
	}}
	if err := (&Client{Installation: "donkey-test"}).stampInstallation(object); err != nil {
		t.Fatal(err)
	}
	if _, found, err := unstructured.NestedFieldNoCopy(object.Object, "spec", "selector"); err != nil || found {
		t.Fatalf("selector-less Service gained a selector: found=%v err=%v", found, err)
	}
}

func TestDestructiveSelectorsAlwaysIncludeInstallationAndLogicalScope(t *testing.T) {
	ctx := context.Background()
	dynamic := dynamicfake.NewSimpleDynamicClient(k8sruntime.NewScheme())
	client := (&Client{Dynamic: dynamic}).WithInstallation("bank-donkey-system")

	if err := client.DeleteAppResources(ctx, "workloads", "team", "site", false); err != nil {
		t.Fatal(err)
	}
	assertDeleteActions(t, dynamic.Actions(), []string{
		"configmaps", "services", "deployments", "secrets", "persistentvolumeclaims",
	}, map[string]string{
		manifests.LabelInstallation: "bank-donkey-system",
		manifests.LabelSpace:        "team",
		manifests.LabelApp:          "site",
	})

	dynamic.ClearActions()
	if err := client.DeleteSpaceObjects(ctx, "workloads", "team"); err != nil {
		t.Fatal(err)
	}
	assertDeleteActions(t, dynamic.Actions(), []string{
		"configmaps", "services", "deployments", "pods", "secrets", "persistentvolumeclaims",
	}, map[string]string{
		manifests.LabelInstallation: "bank-donkey-system",
		manifests.LabelSpace:        "team",
	})

	dynamic.ClearActions()
	if err := client.DeleteSpaceEdges(ctx, "bank-donkey-system", "team"); err != nil {
		t.Fatal(err)
	}
	assertDeleteActions(t, dynamic.Actions(), []string{
		"httproutes", "gateways", "certificates", "secrets",
	}, map[string]string{
		manifests.LabelInstallation: "bank-donkey-system",
		manifests.LabelSpace:        "team",
		manifests.LabelComponent:    manifests.ComponentEdge,
	})
}

func assertDeleteActions(t *testing.T, actions []k8stesting.Action, wantResources []string, wantLabels map[string]string) {
	t.Helper()
	var gotResources []string
	wantSelector := labels.Set(wantLabels).String()
	for _, action := range actions {
		if action.GetVerb() != "delete-collection" {
			continue
		}
		deletion, ok := action.(k8stesting.DeleteCollectionAction)
		if !ok {
			t.Fatalf("%T is not a DeleteCollectionAction", action)
		}
		gotResources = append(gotResources, action.GetResource().Resource)
		if got := deletion.GetListRestrictions().Labels.String(); got != wantSelector {
			t.Errorf("%s selector = %q, want %q", action.GetResource().Resource, got, wantSelector)
		}
	}
	if !reflect.DeepEqual(gotResources, wantResources) {
		t.Fatalf("delete order = %v, want %v (%s)", gotResources, wantResources, fmt.Sprint(actions))
	}
}
