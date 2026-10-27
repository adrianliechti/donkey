package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// ListGatewayConfigMaps returns the installation's routes and access policies
// together so the gateway rebuilds them from the same snapshot.
//
// The installation label is the ownership boundary. Do not select on
// app.kubernetes.io/managed-by here: a route may be managed by Helm or GitOps
// even though donkey-gateway serves it. A route.yaml data entry identifies a
// route; legacy backend annotations remain readable during upgrades.
func (c *Client) ListGatewayConfigMaps(ctx context.Context) ([]corev1.ConfigMap, error) {
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
			if c.ours(cm.Labels) {
				out = append(out, cm)
			}
		}
	}
	return out, nil
}

// ListManagedRoutes returns only routing entries from the gateway snapshot.
func (c *Client) ListManagedRoutes(ctx context.Context) ([]corev1.ConfigMap, error) {
	cms, err := c.ListGatewayConfigMaps(ctx)
	if err != nil {
		return nil, err
	}
	out := []corev1.ConfigMap{}
	for _, cm := range cms {
		if manifests.IsRoute(&cm) {
			out = append(out, cm)
		}
	}
	return out, nil
}

// routeState reads the persisted routing configuration.
func routeState(cm *corev1.ConfigMap) (rt.PathRouteState, error) {
	route, err := manifests.ReadRoute(cm)
	return rt.PathRouteState{
		Name: cm.Name, Host: route.Host, Path: route.Path,
		StripPrefix: route.StripPrefix, Protected: route.Login, Mode: route.Mode, CORS: route.CORS, Access: route.Access,
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
		st.Analytics = route.Analytics
		st.OIDC = route.OIDC
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
// the Deployment) — a patch instead of an
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

func (c *Client) reconcileRouteAccess(ctx context.Context, cm *corev1.ConfigMap, protect bool) error {
	route, err := manifests.ReadRoute(cm)
	if err != nil {
		return err
	}
	route.Access = nil
	if protect {
		route.Login = true
	}
	data := manifests.RouteData(route)
	patchData := map[string]any{manifests.RouteConfigKey: data[manifests.RouteConfigKey], manifests.RouteAccessKey: nil}
	annotations := map[string]any{manifests.AnnotationAccess: nil}
	if protect && cm.Annotations[manifests.AnnotationBackend] != "" {
		annotations[manifests.AnnotationLogin] = manifests.LoginRequired
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": cm.UID, "resourceVersion": cm.ResourceVersion, "annotations": annotations}, "data": patchData,
	})
	if err != nil {
		return err
	}
	_, err = c.Clientset.CoreV1().ConfigMaps(cm.Namespace).Patch(ctx, cm.Name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "donkey-space-access"})
	return err
}
