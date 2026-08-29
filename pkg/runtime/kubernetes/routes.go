package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// ListManagedRoutes returns every route ConfigMap registered for this
// installation, cluster-wide — the donkey-gateway's routing table is built
// from them.
//
// The installation label is the ownership boundary. Do not select on
// app.kubernetes.io/managed-by here: a route may be managed by Helm or GitOps
// even though donkey-gateway serves it. The backend annotation decides whether
// an installation-labelled ConfigMap is actually a route.
func (c *Client) ListManagedRoutes(ctx context.Context) ([]corev1.ConfigMap, error) {
	var out []corev1.ConfigMap
	for _, ns := range c.sweep() {
		list, err := c.Clientset.CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", manifests.LabelInstallation, c.installation()),
		})
		if err != nil {
			return nil, err
		}
		for _, cm := range list.Items {
			// A route another installation owns would put its apps behind this
			// gateway's TLS, login and access rules. Keep this check even though
			// the API-side selector already narrows the list: fake clients and
			// unusual API implementations need not enforce selectors perfectly.
			if cm.Annotations[manifests.AnnotationBackend] != "" && c.ours(cm.Labels) {
				out = append(out, cm)
			}
		}
	}
	return out, nil
}

// routeState reads one route ConfigMap's annotations.
func routeState(cm *corev1.ConfigMap) rt.PathRouteState {
	entry := rt.PathRouteState{
		Name:      cm.Name,
		Host:      cm.Annotations[manifests.AnnotationHost],
		Path:      cm.Annotations[manifests.AnnotationPath],
		Protected: cm.Annotations[manifests.AnnotationLogin] == manifests.LoginRequired,
	}
	if entry.Path == "" {
		entry.Path = "/"
	}
	return entry
}

func (c *Client) AppNetwork(ctx context.Context, namespace, objectName, space, app string) (*rt.NetworkState, error) {
	st := &rt.NetworkState{}
	cm, err := c.Clientset.CoreV1().ConfigMaps(namespace).Get(ctx, objectName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return nil, err
	case !sameObjectIdentity(cm.Labels, c.expectedObjectIdentity(
		manifests.Labels(space, app, manifests.RoleRoute))):
		return nil, c.requireOwnedObject("ConfigMap", namespace, objectName, cm.Labels,
			manifests.Labels(space, app, manifests.RoleRoute))
	case cm.Annotations[manifests.AnnotationBackend] != "":
		st.RouteFound = true
		st.Host = cm.Annotations[manifests.AnnotationHost]
		st.Protected = cm.Annotations[manifests.AnnotationLogin] == manifests.LoginRequired
		if raw := cm.Annotations[manifests.AnnotationAccess]; raw != "" {
			var access config.AccessConfig
			if json.Unmarshal([]byte(raw), &access) == nil {
				st.Access = &access
			}
		}
	}

	routes, err := c.Clientset.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s,%s=%s,%s=%s",
			manifests.LabelInstallation, c.installation(), manifests.LabelSpace, space,
			manifests.LabelApp, app, manifests.LabelRole, manifests.RolePathRoute),
	})
	if err != nil {
		return nil, err
	}
	for i := range routes.Items {
		if c.requireOwnedObject("ConfigMap", namespace, routes.Items[i].Name, routes.Items[i].Labels,
			manifests.Labels(space, app, manifests.RolePathRoute)) != nil {
			continue
		}
		st.Routes = append(st.Routes, routeState(&routes.Items[i]))
	}
	return st, nil
}

// SetAnnotations merge-patches annotations on one object; a nil value removes
// the annotation. Used for the console's own records (donkey.local/console on
// the Deployment) and for access rules on routes — a patch instead of an
// apply, so the field stays out of the deploy's server-side-apply ownership
// and survives the next deploy untouched.
func (c *Client) SetAnnotations(ctx context.Context, kind, namespace, name string, identity map[string]string, annotations map[string]*string) error {
	gvr, ok := gvrFor[kind]
	if !ok {
		return fmt.Errorf("donkey doesn't know how to annotate a %s", kind)
	}
	existing, err := c.Dynamic.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if err := c.requireOwnedObject(kind, namespace, name, existing.GetLabels(), identity); err != nil {
		return err
	}
	patch := map[string]any{
		"metadata": map[string]any{
			"annotations": annotations,
		},
	}
	b, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = c.Dynamic.Resource(gvr).Namespace(namespace).Patch(ctx, name, types.MergePatchType, b, metav1.PatchOptions{})
	return err
}
