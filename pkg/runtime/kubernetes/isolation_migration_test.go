package kubernetes

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

func TestSpaceIsolationMigrationPreservesPolicy(t *testing.T) {
	for _, shared := range []string{"", "workloads"} {
		t.Run("shared="+shared, func(t *testing.T) {
			ctx := context.Background()
			placement := manifests.Placement{NamespacePrefix: "donkey-", SharedNamespace: shared}
			client := &Client{Clientset: fake.NewClientset(), Installation: "main"}
			r := NewRuntime(client, Options{SystemNamespace: "main", Placement: placement})
			old := manifests.SpaceIsolationPolicy("team", placement.Namespace("team"), "main", true)
			old.Name = "donkey.space.team"
			old.UID = "old-policy"
			old.Labels = client.expectedObjectIdentity(old.Labels)
			policies := client.Clientset.NetworkingV1().NetworkPolicies(old.Namespace)
			if _, err := policies.Create(ctx, old, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := r.migrateSpaceIsolation(ctx, "team"); err != nil {
					t.Fatal(err)
				}
			}
			current, err := policies.Get(ctx, "team-space", metav1.GetOptions{})
			if err != nil || !reflect.DeepEqual(current.Spec, old.Spec) || !reflect.DeepEqual(current.Labels, old.Labels) {
				t.Fatal("isolation migration changed the ingress/egress rules or selectors")
			}
			if _, err := policies.Get(ctx, old.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Fatal("legacy policy remains")
			}
		})
	}
}

func TestSpaceIsolationMigrationRetainsOldPolicyOnFailure(t *testing.T) {
	ctx := context.Background()
	old := manifests.SpaceIsolationPolicy("team", "workloads", "main", true)
	old.Name = "donkey.space.team"
	old.Labels[manifests.LabelInstallation] = "main"
	clientset := fake.NewClientset(old)
	clientset.PrependReactor("create", "networkpolicies", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(gvrFor["NetworkPolicy"].GroupResource(), "team-space", fmt.Errorf("rejected"))
	})
	r := NewRuntime(&Client{Clientset: clientset}, Options{SystemNamespace: "main", Placement: manifests.Placement{SharedNamespace: "workloads"}})
	if err := r.migrateSpaceIsolation(ctx, "team"); err == nil {
		t.Fatal("migration ignored the failed replacement")
	}
	if _, err := clientset.NetworkingV1().NetworkPolicies("workloads").Get(ctx, old.Name, metav1.GetOptions{}); err != nil {
		t.Fatal("space lost its isolation after a failed migration")
	}
}

func TestUpgradeRejectsReservedExistingNamesBeforeMutation(t *testing.T) {
	ctx := context.Background()
	// A space called "platform" is donkey's own name; an upgrade must say so
	// before it touches anything.
	marker := manifests.SpaceMarker("platform", "workloads")
	marker.Labels[manifests.LabelInstallation] = "main"
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "platform-db-binding", Namespace: "workloads",
		Labels: manifests.AddonLabels("platform", "db", manifests.RoleBinding),
	}, Data: map[string][]byte{"DATABASE_URL": []byte("keep")}}
	secret.Labels[manifests.LabelInstallation] = "main"
	clientset := fake.NewClientset(marker, secret)
	r := NewRuntime(&Client{Clientset: clientset}, Options{SystemNamespace: "main", Placement: manifests.Placement{SharedNamespace: "workloads"}})
	if err := r.EnsureSpaceConfigs(ctx); err == nil {
		t.Fatal("upgrade accepted a reserved existing space name")
	}
	for _, action := range clientset.Actions() {
		if action.GetVerb() != "get" && action.GetVerb() != "list" {
			t.Fatalf("upgrade mutated resources before rejecting the name: %v", action)
		}
	}
}
