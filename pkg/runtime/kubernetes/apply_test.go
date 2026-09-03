package kubernetes

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

func TestApplyRefusesToAdoptSameNamedForeignObject(t *testing.T) {
	ctx := context.Background()
	existing := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "team--site",
			"namespace": "workloads",
			"labels": map[string]any{
				manifests.LabelInstallation: "another-donkey",
				manifests.LabelSpace:        "team",
				manifests.LabelApp:          "site",
				manifests.LabelRole:         manifests.RoleRoute,
			},
		},
	}}
	dynamic := dynamicfake.NewSimpleDynamicClient(k8sruntime.NewScheme(), existing)
	client := (&Client{Dynamic: dynamic}).WithInstallation("bank-donkey-system")
	desired := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "team--site", Namespace: "workloads",
			Labels: manifests.Labels("team", "site", manifests.RoleRoute),
		},
	}
	if err := client.Apply(ctx, []k8sruntime.Object{desired}); err == nil {
		t.Fatal("force-apply adopted a same-named object owned by another installation")
	}
	for _, action := range dynamic.Actions() {
		if action.GetVerb() == "patch" {
			t.Fatalf("foreign object reached the apply mutation: %v", action)
		}
	}
}

func TestDeleteObjectPreconditionsOnValidatedUID(t *testing.T) {
	ctx := context.Background()
	identity := manifests.Labels("team", "site", manifests.RoleRoute)
	identity[manifests.LabelInstallation] = "bank-donkey-system"
	existing := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name": "team--site", "namespace": "workloads",
			"uid": "validated-object", "labels": mapStringAny(identity),
		},
	}}
	dynamic := dynamicfake.NewSimpleDynamicClient(k8sruntime.NewScheme(), existing)
	client := (&Client{Dynamic: dynamic}).WithInstallation("bank-donkey-system")
	if err := client.DeleteObject(ctx, "ConfigMap", "workloads", "team--site",
		manifests.Labels("team", "site", manifests.RoleRoute)); err != nil {
		t.Fatal(err)
	}
	for _, action := range dynamic.Actions() {
		deleted, ok := action.(k8stesting.DeleteAction)
		if !ok {
			continue
		}
		preconditions := deleted.GetDeleteOptions().Preconditions
		if preconditions == nil || preconditions.UID == nil || *preconditions.UID != "validated-object" {
			t.Fatalf("delete preconditions = %+v", preconditions)
		}
		return
	}
	t.Fatal("no delete action recorded")
}

func mapStringAny(values map[string]string) map[string]any {
	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func TestObjectIdentitySeparatesManagerFromDescriptiveLabels(t *testing.T) {
	expected := manifests.Labels("team", "site", manifests.RoleWorkload)
	expected[manifests.LabelInstallation] = "donkey-system"
	actual := maps.Clone(expected)
	actual["app.kubernetes.io/version"] = "v2"
	if !sameObjectIdentity(actual, expected) {
		t.Fatal("a version change altered immutable identity")
	}
	actual[manifests.LabelManagedBy] = "gitops"
	if !sameObjectIdentity(actual, expected) {
		t.Fatal("descriptive managed-by altered donkey's immutable identity")
	}
	actual["app.kubernetes.io/component"] = "backend"
	if !sameObjectIdentity(actual, expected) {
		t.Fatal("descriptive component altered donkey's immutable identity")
	}
	actual[manifests.LabelRole] = manifests.RoleRoute
	if sameObjectIdentity(actual, expected) {
		t.Fatal("a donkey role change preserved immutable identity")
	}
	actual[manifests.LabelRole] = manifests.RoleWorkload
	actual[manifests.LabelSpace] = "other"
	if sameObjectIdentity(actual, expected) {
		t.Fatal("a logical space change preserved immutable identity")
	}
}

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
			if got := object.GetLabels()[manifests.LabelManagedBy]; got != manifests.ManagedByValue {
				t.Errorf("object managed-by = %q", got)
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
	base := map[string]string{
		manifests.LabelInstallation: "bank-donkey-system",
		manifests.LabelSpace:        "team",
	}
	baseSelector := labels.Set(base).String()
	routeSelector, err := labels.Parse(baseSelector + "," + manifests.LabelRole + "!=" + manifests.RoleSpace)
	if err != nil {
		t.Fatal(err)
	}
	marker := maps.Clone(base)
	marker[manifests.LabelRole] = manifests.RoleSpace
	assertDeleteActionsWithSelectors(t, dynamic.Actions(), []string{
		"configmaps", "services", "deployments", "pods", "secrets", "persistentvolumeclaims",
		"networkpolicies", "configmaps",
	}, []string{
		routeSelector.String(), baseSelector, baseSelector, baseSelector,
		baseSelector, baseSelector, baseSelector, labels.Set(marker).String(),
	})

	dynamic.ClearActions()
	if err := client.DeleteSpaceIsolation(ctx, "workloads", "team"); err != nil {
		t.Fatal(err)
	}
	assertDeleteActions(t, dynamic.Actions(), []string{"networkpolicies"}, map[string]string{
		manifests.LabelInstallation: "bank-donkey-system",
		manifests.LabelSpace:        "team",
		manifests.LabelRole:         manifests.RoleSpace,
	})

	dynamic.ClearActions()
	if err := client.DeleteAppEdges(ctx, "bank-donkey-system", "team", "site"); err != nil {
		t.Fatal(err)
	}
	assertDeleteActions(t, dynamic.Actions(), []string{
		"httproutes", "gateways", "httpproxies", "certificates", "secrets",
	}, map[string]string{
		manifests.LabelInstallation: "bank-donkey-system",
		manifests.LabelSpace:        "team",
		manifests.LabelApp:          "site",
		manifests.LabelRole:         manifests.RoleEdge,
	})

	dynamic.ClearActions()
	if err := client.DeleteSpaceEdges(ctx, "bank-donkey-system", "team"); err != nil {
		t.Fatal(err)
	}
	assertDeleteActions(t, dynamic.Actions(), []string{
		"httproutes", "gateways", "httpproxies", "certificates", "secrets",
	}, map[string]string{
		manifests.LabelInstallation: "bank-donkey-system",
		manifests.LabelSpace:        "team",
		manifests.LabelRole:         manifests.RoleEdge,
	})

	dynamic.ClearActions()
	if err := client.DeleteAppEdgeKinds(ctx, "bank-donkey-system", "team", "site", "HTTPProxy"); err != nil {
		t.Fatal(err)
	}
	assertDeleteActions(t, dynamic.Actions(), []string{"httpproxies"}, map[string]string{
		manifests.LabelInstallation: "bank-donkey-system",
		manifests.LabelSpace:        "team",
		manifests.LabelApp:          "site",
		manifests.LabelRole:         manifests.RoleEdge,
	})

	dynamic.ClearActions()
	if err := client.DeleteSystemContourEdges(ctx, "bank-donkey-system"); err != nil {
		t.Fatal(err)
	}
	assertDeleteActions(t, dynamic.Actions(), []string{"httpproxies", "certificates", "secrets"}, map[string]string{
		manifests.LabelInstallation: "bank-donkey-system",
		manifests.LabelRole:         manifests.SystemRole(manifests.ContourSystemEdgeName),
	})
}

func assertDeleteActions(t *testing.T, actions []k8stesting.Action, wantResources []string, wantLabels map[string]string) {
	t.Helper()
	wantSelector := labels.Set(wantLabels).String()
	wantSelectors := make([]string, len(wantResources))
	for i := range wantSelectors {
		wantSelectors[i] = wantSelector
	}
	assertDeleteActionsWithSelectors(t, actions, wantResources, wantSelectors)
}

func assertDeleteActionsWithSelectors(t *testing.T, actions []k8stesting.Action, wantResources, wantSelectors []string) {
	t.Helper()
	var gotResources []string
	var i int
	for _, action := range actions {
		if action.GetVerb() != "delete-collection" {
			continue
		}
		deletion, ok := action.(k8stesting.DeleteCollectionAction)
		if !ok {
			t.Fatalf("%T is not a DeleteCollectionAction", action)
		}
		gotResources = append(gotResources, action.GetResource().Resource)
		if i >= len(wantSelectors) {
			t.Fatalf("unexpected extra delete action: %v", action)
		}
		if got := deletion.GetListRestrictions().Labels.String(); got != wantSelectors[i] {
			t.Errorf("%s selector = %q, want %q", action.GetResource().Resource, got, wantSelectors[i])
		}
		i++
	}
	if !reflect.DeepEqual(gotResources, wantResources) {
		t.Fatalf("delete order = %v, want %v (%s)", gotResources, wantResources, fmt.Sprint(actions))
	}
}
