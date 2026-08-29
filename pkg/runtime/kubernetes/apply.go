package kubernetes

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
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
	"Pod":                   {Version: "v1", Resource: "pods"},
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
	// Route/config first, then traffic and compute. Secrets and disks stay
	// available until the workloads that use them have been asked to stop.
	"ConfigMap", "Service", "Deployment", "Secret", "PersistentVolumeClaim",
}

var spaceKinds = []string{
	"Service", "Deployment", "Pod", "Secret", "PersistentVolumeClaim",
	// The space's isolation fence goes after the workloads, so it still holds
	// while they drain.
	"NetworkPolicy",
}

var immutableIdentityLabels = []string{
	manifests.LabelInstallation,
	manifests.LabelSpace,
	manifests.LabelApp,
	manifests.LabelAddon,
	manifests.LabelRole,
}

// sameObjectIdentity compares only labels that grant ownership or select a
// logical resource. Every one is donkey.local/*; Kubernetes' standard name,
// instance, component, part-of, managed-by and version labels are descriptive
// metadata and may legitimately change during an apply.
func sameObjectIdentity(actual, expected map[string]string) bool {
	for _, key := range immutableIdentityLabels {
		if actual[key] != expected[key] {
			return false
		}
	}
	return true
}

func requireObjectIdentity(kind, namespace, name string, actual, expected map[string]string) error {
	if sameObjectIdentity(actual, expected) {
		return nil
	}
	qualified := name
	if namespace != "" {
		qualified = namespace + "/" + name
	}
	return fmt.Errorf("the Kubernetes %s %s exists but isn't the expected donkey resource — remove or rename it before continuing", kind, qualified)
}

func (c *Client) expectedObjectIdentity(identity map[string]string) map[string]string {
	expected := maps.Clone(identity)
	if expected == nil {
		expected = map[string]string{}
	}
	expected[manifests.LabelInstallation] = c.installation()
	return expected
}

func (c *Client) requireOwnedObject(kind, namespace, name string, actual, identity map[string]string) error {
	return requireObjectIdentity(kind, namespace, name, actual, c.expectedObjectIdentity(identity))
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
		if err := c.stampInstallation(u); err != nil {
			return err
		}
		kind := u.GetKind()
		gvr, ok := gvrFor[kind]
		if !ok {
			return fmt.Errorf("donkey doesn't know how to apply a %s", kind)
		}
		ri := c.Dynamic.Resource(gvr).Namespace("")
		if !clusterScoped[kind] {
			ri = c.Dynamic.Resource(gvr).Namespace(u.GetNamespace())
		}
		// Force-apply owns donkey's fields, but ownership of the object itself
		// comes only from its immutable identity labels. Without this check a
		// same-named object in a shared namespace would be silently relabelled
		// and later become eligible for deletion by this installation.
		existing, err := ri.Get(ctx, u.GetName(), metav1.GetOptions{})
		if err == nil {
			if err := requireObjectIdentity(kind, u.GetNamespace(), u.GetName(),
				existing.GetLabels(), u.GetLabels()); err != nil {
				return err
			}
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("checking existing %s %s: %w", kind, u.GetName(), err)
		}
		if err := applyWithRetry(ctx, ri, u); err != nil {
			return fmt.Errorf("applying %s %s: %w", kind, u.GetName(), err)
		}
	}
	return nil
}

// stampInstallation marks the object as this installation's, on its way out. It
// happens here rather than in the manifests package so no renderer can forget
// it: an unstamped object is one a second installation would adopt as its own.
func (c *Client) stampInstallation(u *unstructured.Unstructured) error {
	return manifests.StampInstallation(u, c.installation())
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

// DeleteObject removes one named object only after its immutable identity
// matches. Missing objects are fine. The caller gets the name from an owned
// query, but the second check closes the list/delete race.
func (c *Client) DeleteObject(ctx context.Context, kind, namespace, name string, identity map[string]string) error {
	gvr, ok := gvrFor[kind]
	if !ok {
		return fmt.Errorf("donkey doesn't know how to delete a %s", kind)
	}
	ri := c.Dynamic.Resource(gvr).Namespace(namespace)
	existing, err := ri.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := c.requireOwnedObject(kind, namespace, name, existing.GetLabels(), identity); err != nil {
		return err
	}
	deleteOptions := metav1.DeleteOptions{}
	if uid := existing.GetUID(); uid != "" {
		deleteOptions.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	err = ri.Delete(ctx, name, deleteOptions)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// DeleteAppRoute removes the app's own-hostname route by immutable identity.
// The path-route cleanup separately deletes names obtained from an owned label
// query, but this route starts from a deterministic name and therefore must
// not grant that name deletion authority by itself.
func (c *Client) DeleteAppRoute(ctx context.Context, namespace, space, app string) error {
	return c.deleteKindsBySelector(ctx, namespace, []string{"ConfigMap"}, map[string]string{
		manifests.LabelSpace: space,
		manifests.LabelApp:   app,
		manifests.LabelRole:  manifests.RoleRoute,
	}, false)
}

// deleteBySelector removes every namespaced donkey kind matching the label
// selector. keepData keeps the PVCs and the Secrets: a kept database is
// worthless without the credentials that open it (the addon binding Secret
// holds the password the initialized data was created with).
func (c *Client) deleteBySelector(ctx context.Context, namespace string, match map[string]string, keepData bool) error {
	return c.deleteKindsBySelector(ctx, namespace, appKinds, match, keepData)
}

func (c *Client) deleteKindsBySelector(ctx context.Context, namespace string, kinds []string, match map[string]string, keepData bool) error {
	owned := maps.Clone(match)
	if owned == nil {
		owned = map[string]string{}
	}
	owned[manifests.LabelInstallation] = c.installation()
	selector := labels.Set(owned).String()
	for _, kind := range kinds {
		if keepData && (kind == "PersistentVolumeClaim" || kind == "Secret") {
			continue
		}
		if err := c.deleteKindBySelector(ctx, namespace, kind, selector); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) deleteKindBySelector(ctx context.Context, namespace, kind, selector string) error {
	err := c.Dynamic.Resource(gvrFor[kind]).Namespace(namespace).DeleteCollection(ctx,
		metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: selector})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("removing %s objects: %w", kind, err)
	}
	return nil
}

// DeleteAddon removes one space service and optionally keeps its disk.
func (c *Client) DeleteAddon(ctx context.Context, namespace, space, addon string, keepData bool) error {
	return c.deleteBySelector(ctx, namespace, map[string]string{
		manifests.LabelSpace: space,
		manifests.LabelAddon: addon,
	}, keepData)
}

// DetachAddon removes only one app's copy of a space service binding.
func (c *Client) DetachAddon(ctx context.Context, namespace, space, app, addon string) error {
	return c.deleteBySelector(ctx, namespace, map[string]string{
		manifests.LabelSpace: space,
		manifests.LabelApp:   app,
		manifests.LabelAddon: addon,
		manifests.LabelRole:  manifests.RoleBinding,
	}, false)
}
