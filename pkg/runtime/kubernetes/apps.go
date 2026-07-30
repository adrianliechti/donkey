package kubernetes

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// DeleteAppResources deletes every donkey-owned object of the app across the
// fixed kind list. The namespace belongs to the whole space, so it only goes
// away with the space's last app.
func (c *Client) DeleteAppResources(ctx context.Context, namespace, app string, keepData bool) error {
	selector := fmt.Sprintf("%s=%s", manifests.LabelApp, app)
	if err := c.deleteBySelector(ctx, namespace, selector, keepData); err != nil {
		return err
	}
	// Kept data outlives the app, and so does a namespace donkey was given
	// rather than made — where donkey may not even read the namespace to find
	// out, let alone delete it.
	if keepData || c.Confined() {
		return nil
	}
	ns, err := c.Clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if ns.Annotations[manifests.AnnotationManaged] != "true" || !c.ours(ns.Labels) {
		return nil // not this installation's to delete
	}
	// Other apps of the space still live here — leave their home standing.
	remaining, err := c.Clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", manifests.LabelComponent, manifests.ComponentWeb),
	})
	if err != nil {
		return err
	}
	for _, d := range remaining.Items {
		if d.Name != app {
			return nil
		}
	}
	// Data someone chose to keep (an earlier `destroy --keep-data`, an addon
	// removed with its disk kept) must survive the last app leaving too —
	// deleting the namespace would take the PVCs with it. The app's own
	// just-deleted claims still linger in Terminating (pvc-protection
	// finalizer), so only claims that are not already on their way out count.
	kept, err := c.Clientset.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=" + manifests.ManagedByValue,
	})
	if err != nil {
		return err
	}
	for _, pvc := range kept.Items {
		if pvc.DeletionTimestamp == nil {
			return nil
		}
	}
	err = c.Clientset.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// HasDataClaim reports whether the app's data volume exists — used to tell
// the user about a disk left over from an earlier configuration.
func (c *Client) HasDataClaim(ctx context.Context, namespace, app string) bool {
	_, err := c.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, manifests.DataClaimName(app), metav1.GetOptions{})
	return err == nil
}

// SwitchToRecreate clears a Deployment's rolling-update settings so its
// strategy can become Recreate. Kubernetes defaults spec.strategy.rollingUpdate
// for rolling deployments and then refuses that field together with
// type: Recreate — so an app that gains storage (and must stop before its
// replacement starts, since a disk attaches to one machine at a time) would
// otherwise be stuck on its old strategy. A missing Deployment is fine: the
// first deploy renders Recreate directly.
func (c *Client) SwitchToRecreate(ctx context.Context, namespace, name string) error {
	patch := []byte(`{"spec":{"strategy":{"type":"Recreate","rollingUpdate":null}}}`)
	_, err := c.Clientset.AppsV1().Deployments(namespace).Patch(ctx, name,
		types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// AppRef identifies one deployed app.
type AppRef struct {
	Space     string
	App       string
	Namespace string
}

// ListApps finds every deployed donkey app in the cluster, grouped later by
// space. Apps are their Deployments — a namespace alone says nothing now that
// a space holds many apps.
func (c *Client) ListApps(ctx context.Context) ([]AppRef, error) {
	var out []AppRef
	for _, ns := range c.sweep() {
		deps, err := c.Clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app.kubernetes.io/managed-by=%s,%s=%s,%s",
				manifests.ManagedByValue, manifests.LabelComponent, manifests.ComponentWeb, manifests.LabelApp),
		})
		if err != nil {
			return nil, err
		}
		for _, d := range deps.Items {
			name := d.Labels[manifests.LabelApp]
			if name == "" || !c.ours(d.Labels) {
				continue
			}
			out = append(out, AppRef{
				Space:     d.Labels[manifests.LabelSpace],
				App:       name,
				Namespace: d.Namespace,
			})
		}
	}
	return out, nil
}
