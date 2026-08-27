package kubernetes

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
	"k8s.io/client-go/dynamic"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
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
	"Job":                   {Group: "batch", Version: "v1", Resource: "jobs"},
	"NetworkPolicy":         {Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"},
	"Role":                  {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"},
	"RoleBinding":           {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"},
	"ClusterRole":           {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
	"ClusterRoleBinding":    {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},
	// Optional managed edges. Their CRDs are not donkey's to install: the
	// installer preflight refuses a configuration that names them on a
	// cluster that doesn't serve them.
	"HTTPRoute":   {Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"},
	"Gateway":     {Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"},
	"Certificate": {Group: "cert-manager.io", Version: "v1", Resource: "certificates"},
}

var clusterScoped = map[string]bool{"Namespace": true, "ClusterRole": true, "ClusterRoleBinding": true}

// appKinds is everything an app or a space service is ever made of. The
// delete sweeps use it rather than the whole apply table: the other kinds
// there exist for the installation, an app never has one — and an
// installation confined to namespaces it was given holds no permission over
// them, so asking would turn `donkey destroy` into a permission error.
var appKinds = []string{
	"Secret", "ConfigMap", "Service", "PersistentVolumeClaim", "Deployment",
}

// Apply server-side-applies every object with donkey as field manager.
// It is idempotent: create and update are the same operation.
func (c *Client) Apply(ctx context.Context, objs []runtime.Object) error {
	for _, obj := range objs {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return err
		}
		u := &unstructured.Unstructured{Object: m}
		c.stampInstance(u)
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

// stampInstance marks the object as this installation's, on its way out. It
// happens here rather than in the manifests package so no renderer can forget
// it: an unstamped object is one a second installation would adopt as its own.
func (c *Client) stampInstance(u *unstructured.Unstructured) {
	labels := u.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[manifests.LabelInstance] = c.instance()
	u.SetLabels(labels)
}

// applyWithRetry rides out transient control-plane hiccups — most commonly
// an admission webhook (ingress controller) that is briefly unreachable
// while restarting. Permanent errors return immediately.
func applyWithRetry(ctx context.Context, ri dynamic.ResourceInterface, u *unstructured.Unstructured) error {
	var lastErr error
	for range 10 {
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
// e.g. the route ConfigMap when an app turns internal).
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

// deleteBySelector removes every namespaced donkey kind matching the label
// selector. keepData keeps the PVCs and the Secrets: a kept database is
// worthless without the credentials that open it (the addon binding Secret
// holds the password the initialized data was created with).
func (c *Client) deleteBySelector(ctx context.Context, namespace, selector string, keepData bool) error {
	for _, kind := range appKinds {
		if keepData && (kind == "PersistentVolumeClaim" || kind == "Secret") {
			continue
		}
		err := c.Dynamic.Resource(gvrFor[kind]).Namespace(namespace).DeleteCollection(ctx,
			metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: selector})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("removing %s objects: %w", kind, err)
		}
	}
	return nil
}

// DeleteComponent removes one component's objects (e.g. an addon), keeping
// the PVC when keepData is set.
func (c *Client) DeleteComponent(ctx context.Context, namespace, app, component string, keepData bool) error {
	return c.deleteBySelector(ctx, namespace,
		fmt.Sprintf("%s=%s,%s=%s", manifests.LabelApp, app, manifests.LabelComponent, component), keepData)
}

// DetachAddon removes an app's copy of a shared addon's binding — only the
// copy: the shared marker keeps an app-owned addon of the same name safe.
func (c *Client) DetachAddon(ctx context.Context, namespace, app, component string) error {
	return c.deleteBySelector(ctx, namespace,
		fmt.Sprintf("%s=%s,%s=%s,%s=true", manifests.LabelApp, app, manifests.LabelComponent, component, manifests.LabelShared), false)
}
