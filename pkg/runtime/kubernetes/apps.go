package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// DeleteAppResources deletes only one logical app. A space is a separate
// lifecycle boundary and is removed only through DeleteSpace.
func (c *Client) DeleteAppResources(ctx context.Context, namespace, space, app string, keepData bool) error {
	return c.deleteBySelector(ctx, namespace, map[string]string{
		manifests.LabelSpace: space,
		manifests.LabelApp:   app,
	}, keepData)
}

// HasDataClaim reports whether the app's data volume exists — used to tell
// the user about a disk left over from an earlier configuration.
func (c *Client) HasDataClaim(ctx context.Context, namespace, objectName, space, app string) bool {
	pvc, err := c.dataClaim(ctx, namespace, manifests.DataClaimName(objectName), manifests.Labels(space, app, manifests.RoleStorage))
	return err == nil && c.requireOwnedObject("PersistentVolumeClaim", namespace, pvc.Name, pvc.Labels,
		manifests.Labels(space, app, manifests.RoleStorage)) == nil
}

// SwitchToRecreate clears a Deployment's rolling-update settings so its
// strategy can become Recreate. Kubernetes defaults spec.strategy.rollingUpdate
// for rolling deployments and then refuses that field together with
// type: Recreate — so an app that gains storage (and must stop before its
// replacement starts, since a disk attaches to one machine at a time) would
// otherwise be stuck on its old strategy. A missing Deployment is fine: the
// first deploy renders Recreate directly.
func (c *Client) SwitchToRecreate(ctx context.Context, namespace, name, space, app string) error {
	dep, err := c.Clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := c.requireOwnedObject("Deployment", namespace, name, dep.Labels,
		manifests.Labels(space, app, manifests.RoleWorkload)); err != nil {
		return err
	}
	patch := []byte(`{"spec":{"strategy":{"type":"Recreate","rollingUpdate":null}}}`)
	_, err = c.Clientset.AppsV1().Deployments(namespace).Patch(ctx, name,
		types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	return err
}

// SetAppNetworkMetadata records network choices on the workload as well as
// its route. Internal apps have no own route, so the Deployment is the source
// that lets status and the console retain their mode and CORS policy.
func (c *Client) SetAppNetworkMetadata(ctx context.Context, namespace, name, space, app, mode, cors string) error {
	dep, err := c.Clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if err := c.requireOwnedObject("Deployment", namespace, name, dep.Labels,
		manifests.Labels(space, app, manifests.RoleWorkload)); err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{
			manifests.AnnotationMode: mode,
			manifests.AnnotationCORS: cors,
		}},
	})
	if err != nil {
		return err
	}
	_, err = c.Clientset.AppsV1().Deployments(namespace).Patch(ctx, name,
		types.StrategicMergePatchType, patch, metav1.PatchOptions{})
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
			LabelSelector: fmt.Sprintf("%s=%s,%s=%s,%s",
				manifests.LabelInstallation, c.installation(),
				manifests.LabelRole, manifests.RoleWorkload, manifests.LabelApp),
		})
		if err != nil {
			return nil, err
		}
		for _, d := range deps.Items {
			space, name := d.Labels[manifests.LabelSpace], d.Labels[manifests.LabelApp]
			if space == "" || name == "" || c.requireOwnedObject("Deployment", d.Namespace, d.Name, d.Labels,
				manifests.Labels(space, name, manifests.RoleWorkload)) != nil {
				continue
			}
			out = append(out, AppRef{
				Space:     space,
				App:       name,
				Namespace: d.Namespace,
			})
		}
	}
	return out, nil
}
