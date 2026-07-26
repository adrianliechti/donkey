package k8s

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/k8s/manifests"
)

// ListManagedRoutes returns every HTTPRoute donkey rendered, cluster-wide —
// the donkey-gateway's routing table is built from them.
func (c *Client) ListManagedRoutes(ctx context.Context) ([]unstructured.Unstructured, error) {
	list, err := c.Dynamic.Resource(gvrFor["HTTPRoute"]).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=" + manifests.ManagedByValue,
	})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// PathRouteState is one shared-address route as it exists on the cluster.
type PathRouteState struct {
	Name string
	Host string
	Path string
	// Protected says the route sends its traffic through the login gateway.
	Protected bool
}

// NetworkState is what the cluster currently says about how an app is
// reached — read from the objects, never from stored intent, so the console
// shows what is true rather than what was meant.
type NetworkState struct {
	// RouteFound: the app has an address of its own (not internal).
	RouteFound bool
	Host       string
	Protected  bool
	// Access are the rules the gateway enforces on the app's own route.
	Access *config.AccessConfig
	Routes []PathRouteState
}

func (c *Client) AppNetwork(ctx context.Context, namespace, app string) (*NetworkState, error) {
	st := &NetworkState{}
	route, err := c.Dynamic.Resource(gvrFor["HTTPRoute"]).Namespace(namespace).Get(ctx, app, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return nil, err
	default:
		st.RouteFound = true
		if hosts, ok, _ := unstructured.NestedStringSlice(route.Object, "spec", "hostnames"); ok && len(hosts) > 0 {
			st.Host = hosts[0]
		}
		st.Protected = route.GetAnnotations()[manifests.AnnotationLogin] == manifests.LoginRequired
		if raw := route.GetAnnotations()[manifests.AnnotationAccess]; raw != "" {
			var access config.AccessConfig
			if json.Unmarshal([]byte(raw), &access) == nil {
				st.Access = &access
			}
		}
	}

	routes, err := c.Dynamic.Resource(gvrFor["HTTPRoute"]).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: manifests.LabelApp + "=" + app + "," + manifests.LabelComponent + "=" + manifests.ComponentRoute,
	})
	if err != nil {
		return nil, err
	}
	for _, r := range routes.Items {
		entry := PathRouteState{
			Name:      r.GetName(),
			Path:      "/",
			Protected: r.GetAnnotations()[manifests.AnnotationLogin] == manifests.LoginRequired,
		}
		if hosts, ok, _ := unstructured.NestedStringSlice(r.Object, "spec", "hostnames"); ok && len(hosts) > 0 {
			entry.Host = hosts[0]
		}
		if rules, _, _ := unstructured.NestedSlice(r.Object, "spec", "rules"); len(rules) > 0 {
			if rule, ok := rules[0].(map[string]any); ok {
				if matches, _, _ := unstructured.NestedSlice(rule, "matches"); len(matches) > 0 {
					if m, ok := matches[0].(map[string]any); ok {
						if v, _, _ := unstructured.NestedString(m, "path", "value"); v != "" {
							entry.Path = v
						}
					}
				}
			}
		}
		st.Routes = append(st.Routes, entry)
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
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	if err != nil {
		return err
	}
	_, err = c.Dynamic.Resource(gvr).Namespace(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// RouteAccepted reports whether the Gateway accepted an HTTPRoute and
// resolved its backends. For a protected app this is the promise check: the
// route either points into the login gateway or it doesn't serve.
func (c *Client) RouteAccepted(ctx context.Context, namespace, name string) (bool, string, error) {
	route, err := c.Dynamic.Resource(gvrFor["HTTPRoute"]).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, "", err
	}
	parents, _, err := unstructured.NestedSlice(route.Object, "status", "parents")
	if err != nil {
		return false, "", err
	}
	for _, item := range parents {
		parent, ok := item.(map[string]any)
		if !ok {
			continue
		}
		conditions, _, _ := unstructured.NestedSlice(parent, "conditions")
		accepted, resolved := false, false
		var detail string
		for _, ci := range conditions {
			cond, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			status := cond["status"] == "True"
			switch cond["type"] {
			case "Accepted":
				accepted = status
			case "ResolvedRefs":
				resolved = status
			}
			if !status {
				if msg, _ := cond["message"].(string); msg != "" {
					detail = msg
				} else if reason, _ := cond["reason"].(string); reason != "" {
					detail = reason
				}
			}
		}
		if accepted && resolved {
			return true, "", nil
		}
		if detail != "" {
			return false, detail, nil
		}
	}
	return false, "no controller has picked it up yet", nil
}
