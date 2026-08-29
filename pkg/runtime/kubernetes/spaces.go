package kubernetes

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// DeleteSpace removes an owned space namespace and everything in it. The
// namespace ownership annotation is the deletion authority; labels alone are
// never enough to make somebody else's namespace deletable.
func (c *Client) DeleteSpace(ctx context.Context, namespace, space string) error {
	ns, err := c.Clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if ns.Annotations[manifests.AnnotationManaged] != "true" || !c.ours(ns.Labels) ||
		ns.Labels[manifests.LabelSpace] != space ||
		ns.Labels[manifests.LabelRole] != manifests.RoleSpace {
		return fmt.Errorf("that namespace isn't this installation's to delete")
	}
	deleteOptions := metav1.DeleteOptions{}
	if uid := ns.UID; uid != "" {
		deleteOptions.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	err = c.Clientset.CoreV1().Namespaces().Delete(ctx, namespace, deleteOptions)
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// DeleteSpaceObjects removes a logical space from a shared namespace. The
// installation + space selector keeps every other tenant and installation
// outside the deletion boundary.
func (c *Client) DeleteSpaceObjects(ctx context.Context, namespace, space string) error {
	owned := map[string]string{
		manifests.LabelInstallation: c.installation(),
		manifests.LabelSpace:        space,
	}
	// Stop routing first, but preserve the space marker. The `!=` selector
	// also matches externally managed route records with no role label.
	// Keeping the marker until every later deletion succeeds makes a partial
	// cascade visible and retryable after a UI refresh.
	routes := labels.Set(owned).String() + "," + manifests.LabelRole + "!=" + manifests.RoleSpace
	if err := c.deleteKindBySelector(ctx, namespace, "ConfigMap", routes); err != nil {
		return err
	}
	if err := c.deleteKindsBySelector(ctx, namespace, spaceKinds,
		map[string]string{manifests.LabelSpace: space}, false); err != nil {
		return err
	}
	return c.deleteKindsBySelector(ctx, namespace, []string{"ConfigMap"}, map[string]string{
		manifests.LabelSpace: space,
		manifests.LabelRole:  manifests.RoleSpace,
	}, false)
}

// DeleteSpaceIsolation retracts only the space's network fence when an
// installation turns isolation off. The deterministic policy name is not
// deletion authority; installation + logical space + role are.
func (c *Client) DeleteSpaceIsolation(ctx context.Context, namespace, space string) error {
	return c.deleteKindsBySelector(ctx, namespace, []string{"NetworkPolicy"}, map[string]string{
		manifests.LabelSpace: space,
		manifests.LabelRole:  manifests.RoleSpace,
	}, false)
}

// DeleteSpaceEdges retracts cross-namespace Gateway API and cert-manager
// objects before their workload namespace disappears. The generated TLS
// Secret receives the same labels through Certificate.spec.secretTemplate.
func (c *Client) DeleteSpaceEdges(ctx context.Context, namespace, space string) error {
	return c.deleteKindsBySelector(ctx, namespace,
		[]string{"HTTPRoute", "Gateway", "Certificate", "Secret"},
		map[string]string{
			manifests.LabelSpace: space,
			manifests.LabelRole:  manifests.RoleEdge,
		}, false)
}

// DeleteAppEdges retracts one app's cross-namespace edge. Labels, rather than
// deterministic names, are the deletion authority: a collision with an
// object written by somebody else must be reported by apply, never erased by
// cleanup.
func (c *Client) DeleteAppEdges(ctx context.Context, namespace, space, app string) error {
	return c.deleteKindsBySelector(ctx, namespace,
		[]string{"HTTPRoute", "Gateway", "Certificate", "Secret"},
		map[string]string{
			manifests.LabelSpace: space,
			manifests.LabelApp:   app,
			manifests.LabelRole:  manifests.RoleEdge,
		}, false)
}

// ListSpaces finds every donkey space namespace, including ones holding no
// app — a space can carry only services now, and it must not vanish from
// view for that.
func (c *Client) ListSpaces(ctx context.Context) ([]AppRef, error) {
	namespaces, err := c.Clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s,%s", manifests.LabelInstallation, c.installation(),
			manifests.LabelRole, manifests.RoleSpace, manifests.LabelSpace),
	})
	if err != nil {
		return nil, err
	}
	var out []AppRef
	for _, ns := range namespaces.Items {
		if ns.Name == c.installation() || ns.DeletionTimestamp != nil {
			continue
		}
		space := ns.Labels[manifests.LabelSpace]
		if space == "" || ns.Annotations[manifests.AnnotationManaged] != "true" ||
			c.requireOwnedObject("Namespace", "", ns.Name, ns.Labels, manifests.SpaceLabels(space)) != nil {
			continue
		}
		out = append(out, AppRef{Space: space, Namespace: ns.Name})
	}
	return out, nil
}

// ListSpaceMarkers finds the spaces of an installation that shares one
// namespace, where a space is an object rather than a namespace. A space with
// apps has them; one holding only services — or nothing at all, freshly made
// in the console — has only this.
func (c *Client) ListSpaceMarkers(ctx context.Context, namespace string) ([]AppRef, error) {
	cms, err := c.Clientset.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s,%s", manifests.LabelInstallation, c.installation(),
			manifests.LabelRole, manifests.RoleSpace, manifests.LabelSpace),
	})
	if err != nil {
		return nil, err
	}
	var out []AppRef
	for _, cm := range cms.Items {
		space := cm.Labels[manifests.LabelSpace]
		if space == "" || c.requireOwnedObject("ConfigMap", namespace, cm.Name, cm.Labels,
			manifests.SpaceLabels(space)) != nil {
			continue
		}
		out = append(out, AppRef{Space: space, Namespace: namespace})
	}
	return out, nil
}

// NamespaceLabels returns a namespace's labels and whether it exists at all.
// The installer reads them to find out what Pod Security Standard a namespace
// it was given enforces — the one thing that silently stops donkey after a
// clean install, because admission rejects the pod and the Deployment just
// never produces one.
func (c *Client) NamespaceLabels(ctx context.Context, name string) (map[string]string, bool, error) {
	ns, err := c.Clientset.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return ns.Labels, true, nil
}

// CheckNamespaceFree refuses to move into a namespace that exists but isn't
// donkey's — a space name must never let donkey take over (or later delete)
// somebody else's namespace. Only the managed annotation counts: a label
// somebody put on their own namespace must not turn it into one donkey may
// delete.
func (c *Client) CheckNamespaceFree(ctx context.Context, namespace, space string) error {
	// The control plane's namespace carries the managed annotation too — the
	// space "system" must not adopt it (and must never be deletable as a
	// space).
	if namespace == c.installation() {
		return fmt.Errorf("the space name maps to donkey's own namespace %s — pick a different space name (space: <name> in donkey.yaml)", namespace)
	}
	ns, err := c.Clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if ns.Annotations[manifests.AnnotationManaged] == "true" {
		// Managed says a donkey made it — not that this one did. Two
		// installations with the same prefix map a space onto the same
		// namespace, and adopting the other's would rewrite its labels
		// (server-side apply forces them) and make it deletable from here.
		if c.ours(ns.Labels) && ns.Labels[manifests.LabelSpace] == space &&
			ns.Labels[manifests.LabelRole] == manifests.RoleSpace {
			return nil
		}
		if c.ours(ns.Labels) {
			return fmt.Errorf("the namespace %s is this installation's, but represents space %q rather than %q — remove or repair it before continuing", namespace, ns.Labels[manifests.LabelSpace], space)
		}
		return fmt.Errorf("the namespace %s belongs to another donkey installation — pick a different space name (space: <name> in donkey.yaml)", namespace)
	}
	return fmt.Errorf("the namespace %s already exists and wasn't created by donkey — pick a different space name (space: <name> in donkey.yaml)", namespace)
}
