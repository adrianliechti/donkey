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

// ListManagedRoutes returns every route ConfigMap donkey rendered,
// cluster-wide — the donkey-gateway's routing table is built from them.
// Only ConfigMaps carrying the backend annotation are routes; the label
// narrows the sweep, the annotation decides.
func (c *Client) ListManagedRoutes(ctx context.Context) ([]corev1.ConfigMap, error) {
	var out []corev1.ConfigMap
	for _, ns := range c.sweep() {
		list, err := c.Clientset.CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/managed-by=" + manifests.ManagedByValue,
		})
		if err != nil {
			return nil, err
		}
		for _, cm := range list.Items {
			// managed-by says a donkey wrote it, not that this one did — and a
			// route another installation owns would put its apps behind this
			// gateway's TLS, login and access rules.
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

func (c *Client) AppNetwork(ctx context.Context, namespace, app string) (*rt.NetworkState, error) {
	st := &rt.NetworkState{}
	cm, err := c.Clientset.CoreV1().ConfigMaps(namespace).Get(ctx, app, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return nil, err
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
		LabelSelector: manifests.LabelApp + "=" + app + "," + manifests.LabelComponent + "=" + manifests.ComponentRoute,
	})
	if err != nil {
		return nil, err
	}
	for i := range routes.Items {
		st.Routes = append(st.Routes, routeState(&routes.Items[i]))
	}
	return st, nil
}

// SetAnnotations merge-patches annotations on one object; a nil value removes
// the annotation. Used for the console's own records (donkey.dev/console on
// the Deployment) and for access rules on routes — a patch instead of an
// apply, so the field stays out of the deploy's server-side-apply ownership
// and survives the next deploy untouched.
func (c *Client) SetAnnotations(ctx context.Context, kind, namespace, name string, annotations map[string]*string) error {
	gvr, ok := gvrFor[kind]
	if !ok {
		return fmt.Errorf("donkey doesn't know how to annotate a %s", kind)
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
