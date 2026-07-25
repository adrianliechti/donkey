package k8s

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"monobox.ch/donkey/internal/k8s/manifests"
)

const fieldManager = "donkey"

// gvrFor maps the fixed set of kinds donkey manages. A hand-rolled table
// avoids a discovery round-trip per command; donkey owns its objects
// outright so the set is closed.
var gvrFor = map[string]schema.GroupVersionResource{
	"Namespace":             {Version: "v1", Resource: "namespaces"},
	"Secret":                {Version: "v1", Resource: "secrets"},
	"ConfigMap":             {Version: "v1", Resource: "configmaps"},
	"Service":               {Version: "v1", Resource: "services"},
	"ServiceAccount":        {Version: "v1", Resource: "serviceaccounts"},
	"PersistentVolumeClaim": {Version: "v1", Resource: "persistentvolumeclaims"},
	"Deployment":            {Group: "apps", Version: "v1", Resource: "deployments"},
	"ClusterRoleBinding":    {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},
	"GatewayClass":          {Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gatewayclasses"},
	"Gateway":               {Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"},
	"HTTPRoute":             {Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"},
	"SecurityPolicy":        {Group: "gateway.envoyproxy.io", Version: "v1alpha1", Resource: "securitypolicies"},
	"Certificate":           {Group: "cert-manager.io", Version: "v1", Resource: "certificates"},
}

var clusterScoped = map[string]bool{"Namespace": true, "ClusterRoleBinding": true, "GatewayClass": true}

// Apply server-side-applies every object with donkey as field manager.
// It is idempotent: create and update are the same operation.
func (c *Client) Apply(ctx context.Context, objs []runtime.Object) error {
	for _, obj := range objs {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return err
		}
		u := &unstructured.Unstructured{Object: m}
		kind := u.GetKind()
		gvr, ok := gvrFor[kind]
		if !ok {
			return fmt.Errorf("donkey doesn't know how to apply a %s", kind)
		}
		ri := c.Dynamic.Resource(gvr).Namespace("")
		if !clusterScoped[kind] {
			ri = c.Dynamic.Resource(gvr).Namespace(u.GetNamespace())
		}
		if err := applyWithRetry(ctx, ri, u); err != nil {
			return fmt.Errorf("applying %s %s: %w", kind, u.GetName(), err)
		}
	}
	return nil
}

// applyWithRetry rides out transient control-plane hiccups — most commonly
// an admission webhook (ingress controller) that is briefly unreachable
// while restarting. Permanent errors return immediately.
func applyWithRetry(ctx context.Context, ri dynamic.ResourceInterface, u *unstructured.Unstructured) error {
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		_, err := ri.Apply(ctx, u.GetName(), u, metav1.ApplyOptions{FieldManager: fieldManager, Force: true})
		if err == nil {
			return nil
		}
		if !isTransient(err) {
			return err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return lastErr
}

func isTransient(err error) bool {
	if apierrors.IsInternalError(err) || apierrors.IsServiceUnavailable(err) ||
		apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) || apierrors.IsTimeout(err) {
		return true
	}
	return strings.Contains(err.Error(), "failed calling webhook")
}

// DeleteObject removes a single named object of a known kind; missing
// objects are fine (used to retract objects an apply no longer includes,
// e.g. the SecurityPolicy when an app turns public).
func (c *Client) DeleteObject(ctx context.Context, kind, namespace, name string) error {
	gvr, ok := gvrFor[kind]
	if !ok {
		return fmt.Errorf("donkey doesn't know how to delete a %s", kind)
	}
	err := c.Dynamic.Resource(gvr).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// DeleteComponent removes one component's objects (e.g. an addon), keeping
// the PVC when keepData is set.
func (c *Client) DeleteComponent(ctx context.Context, namespace, app, component string, keepData bool) error {
	selector := fmt.Sprintf("donkey.dev/app=%s,donkey.dev/component=%s", app, component)
	for kind, gvr := range gvrFor {
		if clusterScoped[kind] {
			continue
		}
		if keepData && kind == "PersistentVolumeClaim" {
			continue
		}
		err := c.Dynamic.Resource(gvr).Namespace(namespace).DeleteCollection(ctx,
			metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: selector})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("removing %s objects: %w", kind, err)
		}
	}
	return nil
}

// DeleteAppResources deletes every donkey-owned object of the app across the
// fixed kind list. The namespace belongs to the whole space, so it only goes
// away with the space's last app.
func (c *Client) DeleteAppResources(ctx context.Context, namespace, app string, keepData bool) error {
	selector := fmt.Sprintf("%s=%s", manifests.LabelApp, app)
	for kind, gvr := range gvrFor {
		if clusterScoped[kind] {
			continue
		}
		if keepData && kind == "PersistentVolumeClaim" {
			continue
		}
		err := c.Dynamic.Resource(gvr).Namespace(namespace).DeleteCollection(ctx,
			metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: selector})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("removing %s objects: %w", kind, err)
		}
	}
	if keepData {
		return nil
	}
	ns, err := c.Clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if ns.Annotations[manifests.AnnotationManaged] != "true" {
		return nil // not ours to delete
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
	err = c.Clientset.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
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
	deps, err := c.Clientset.AppsV1().Deployments("").List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app.kubernetes.io/managed-by=%s,%s=%s,%s",
			manifests.ManagedByValue, manifests.LabelComponent, manifests.ComponentWeb, manifests.LabelApp),
	})
	if err != nil {
		return nil, err
	}
	var out []AppRef
	for _, d := range deps.Items {
		name := d.Labels[manifests.LabelApp]
		if name == "" {
			continue
		}
		out = append(out, AppRef{
			Space:     d.Labels[manifests.LabelSpace],
			App:       name,
			Namespace: d.Namespace,
		})
	}
	return out, nil
}

// CheckNamespaceFree refuses to move into a namespace that exists but isn't
// donkey's — a space name must never let donkey take over (or later delete)
// somebody else's namespace.
func (c *Client) CheckNamespaceFree(ctx context.Context, namespace, space string) error {
	ns, err := c.Clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if ns.Annotations[manifests.AnnotationManaged] == "true" || ns.Labels[manifests.LabelSpace] == space {
		return nil
	}
	return fmt.Errorf("the namespace %s already exists and wasn't created by donkey — pick a different space name (space: <name> in donkey.yaml)", namespace)
}
