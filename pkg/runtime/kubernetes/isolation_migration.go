package kubernetes

import (
	"context"
	"maps"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// Preserve the live ingress and egress rules during a rename. Installation
// reconciliation separately applies changes to the isolation settings.
func (r *Runtime) migrateSpaceIsolation(ctx context.Context, space string) error {
	policies := r.c.Clientset.NetworkingV1().NetworkPolicies(r.ns(space))
	old, err := policies.Get(ctx, "donkey.space."+space, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	identity := manifests.SpaceLabels(space)
	if err := r.c.requireOwnedObject("NetworkPolicy", old.Namespace, old.Name, old.Labels, identity); err != nil {
		return err
	}
	fresh := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: manifests.SpaceIsolationName(space), Namespace: old.Namespace,
		Labels: maps.Clone(old.Labels), Annotations: maps.Clone(old.Annotations),
	}, Spec: *old.Spec.DeepCopy()}
	stored, err := policies.Create(ctx, fresh, metav1.CreateOptions{FieldManager: "donkey"})
	if apierrors.IsAlreadyExists(err) {
		stored, err = policies.Get(ctx, fresh.Name, metav1.GetOptions{})
	}
	if err != nil {
		return err
	}
	if err := r.c.requireOwnedObject("NetworkPolicy", stored.Namespace, stored.Name, stored.Labels, identity); err != nil {
		return err
	}
	opts := metav1.DeleteOptions{}
	if old.UID != "" {
		opts.Preconditions = &metav1.Preconditions{UID: &old.UID}
	}
	if err := policies.Delete(ctx, old.Name, opts); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
