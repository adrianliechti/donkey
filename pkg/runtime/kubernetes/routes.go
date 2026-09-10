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
			if manifests.IsRoute(&cm) && c.ours(cm.Labels) {
				out = append(out, cm)
			}
		}
	}
	return out, nil
}

// routeState reads the persisted routing configuration.
func routeState(cm *corev1.ConfigMap) (rt.PathRouteState, error) {
	route, err := manifests.ReadRoute(cm)
	return rt.PathRouteState{
		Name: cm.Name, Host: route.Host, Path: route.Path,
		StripPrefix: route.StripPrefix, Protected: route.Login, Mode: route.Mode, CORS: route.CORS,
	}, err
}

func (c *Client) appRouteConfigMap(ctx context.Context, namespace, objectName, space, app string) (*corev1.ConfigMap, error) {
	cms := c.Clientset.CoreV1().ConfigMaps(namespace)
	cm, err := cms.Get(ctx, manifests.AppRouteName(objectName), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		cm, err = cms.Get(ctx, objectName, metav1.GetOptions{})
		if err == nil && objectName == manifests.SpaceMarkerName(space) &&
			c.requireOwnedObject("ConfigMap", namespace, cm.Name, cm.Labels, manifests.SpaceLabels(space)) == nil {
			return nil, apierrors.NewNotFound(corev1.Resource("configmaps"), manifests.AppRouteName(objectName))
		}
	}
	if err != nil {
		return nil, err
	}
	if err := c.requireOwnedObject("ConfigMap", namespace, cm.Name, cm.Labels, manifests.Labels(space, app, manifests.RoleRoute)); err != nil {
		return nil, err
	}
	return cm, nil
}

func (c *Client) AppNetwork(ctx context.Context, namespace, objectName, space, app string) (*rt.NetworkState, error) {
	st := &rt.NetworkState{}
	cm, err := c.appRouteConfigMap(ctx, namespace, objectName, space, app)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return nil, err
	case !sameObjectIdentity(cm.Labels, c.expectedObjectIdentity(
		manifests.Labels(space, app, manifests.RoleRoute))):
		return nil, c.requireOwnedObject("ConfigMap", namespace, objectName, cm.Labels,
			manifests.Labels(space, app, manifests.RoleRoute))
	default:
		route, err := manifests.ReadRoute(cm)
		if err != nil {
			return nil, err
		}
		st.RouteFound = route.Backend != ""
		st.Host, st.Protected, st.Mode, st.CORS, st.Access = route.Host, route.Login, route.Mode, route.CORS, route.Access
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
		entry, err := routeState(&routes.Items[i])
		if err != nil {
			return nil, err
		}
		st.Routes = append(st.Routes, entry)
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

// SetRouteAccess patches a separate config key; ordinary route deploys do not
// overwrite access settings maintained in the console.
func (c *Client) SetRouteAccess(ctx context.Context, namespace, name string, identity map[string]string, access *config.AccessConfig) error {
	cms := c.Clientset.CoreV1().ConfigMaps(namespace)
	cm, err := cms.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if err := c.requireOwnedObject("ConfigMap", namespace, name, cm.Labels, identity); err != nil {
		return err
	}
	var value *string
	if access != nil {
		raw, err := config.EncodeYAML(access)
		if err != nil {
			return err
		}
		encoded := string(raw)
		value = &encoded
	}
	metadata := map[string]any{"uid": cm.UID}
	if cm.Annotations[manifests.AnnotationBackend] != "" {
		// Keep an older gateway's reader consistent during the rolling upgrade.
		var legacy *string
		if access != nil {
			raw, _ := json.Marshal(access)
			encoded := string(raw)
			legacy = &encoded
		}
		metadata["annotations"] = map[string]*string{manifests.AnnotationAccess: legacy}
	}
	patch, err := json.Marshal(map[string]any{"metadata": metadata, "data": map[string]*string{manifests.RouteAccessKey: value}})
	if err != nil {
		return err
	}
	_, err = cms.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "donkey-route-access"})
	return err
}
