package kubernetes

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// DeleteSpace removes an app-less space: its namespace, and with it any
// services and their data. Spaces that still hold apps keep their own
// removal flow (with its own warnings) — this refuses them.
func (c *Client) DeleteSpace(ctx context.Context, namespace string) error {
	ns, err := c.Clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if ns.Annotations[manifests.AnnotationManaged] != "true" || !c.ours(ns.Labels) {
		return fmt.Errorf("that namespace isn't this installation's to delete")
	}
	apps, err := c.Clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", manifests.LabelComponent, manifests.ComponentWeb),
	})
	if err != nil {
		return err
	}
	if n := len(apps.Items); n > 0 {
		return fmt.Errorf("the space still holds %d %s — remove them first", n, plural(n, "app"))
	}
	err = c.Clientset.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// DeleteSpaceObjects removes an app-less space that has no namespace of its
// own: its marker and everything else carrying its label — the services it
// held and their disks, which is what deleting the namespace did before.
// Spaces that still hold apps keep their own removal flow; this refuses them.
func (c *Client) DeleteSpaceObjects(ctx context.Context, namespace, space string) error {
	apps, err := c.Clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s",
			manifests.LabelSpace, space, manifests.LabelComponent, manifests.ComponentWeb),
	})
	if err != nil {
		return err
	}
	if n := len(apps.Items); n > 0 {
		return fmt.Errorf("the space still holds %d %s — remove them first", n, plural(n, "app"))
	}
	return c.deleteBySelector(ctx, namespace, fmt.Sprintf("%s=%s,app.kubernetes.io/managed-by=%s",
		manifests.LabelSpace, space, manifests.ManagedByValue), false)
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// ListSpaces finds every donkey space namespace, including ones holding no
// app — a space can carry only services now, and it must not vanish from
// view for that.
func (c *Client) ListSpaces(ctx context.Context) ([]AppRef, error) {
	namespaces, err := c.Clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: manifests.LabelSpace,
	})
	if err != nil {
		return nil, err
	}
	var out []AppRef
	for _, ns := range namespaces.Items {
		if ns.Name == c.instance() || ns.DeletionTimestamp != nil {
			continue
		}
		if ns.Annotations[manifests.AnnotationManaged] != "true" || !c.ours(ns.Labels) {
			continue
		}
		out = append(out, AppRef{Space: ns.Labels[manifests.LabelSpace], Namespace: ns.Name})
	}
	return out, nil
}

// ListSpaceMarkers finds the spaces of an installation that shares one
// namespace, where a space is an object rather than a namespace. A space with
// apps has them; one holding only services — or nothing at all, freshly made
// in the console — has only this.
func (c *Client) ListSpaceMarkers(ctx context.Context, namespace string) ([]AppRef, error) {
	cms, err := c.Clientset.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s", manifests.LabelComponent, manifests.ComponentSpace, manifests.LabelSpace),
	})
	if err != nil {
		return nil, err
	}
	var out []AppRef
	for _, cm := range cms.Items {
		space := cm.Labels[manifests.LabelSpace]
		if space == "" || !c.ours(cm.Labels) {
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
func (c *Client) CheckNamespaceFree(ctx context.Context, namespace string) error {
	// The control plane's namespace carries the managed annotation too — the
	// space "system" must not adopt it (and must never be deletable as a
	// space).
	if namespace == c.instance() {
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
		if c.ours(ns.Labels) {
			return nil
		}
		return fmt.Errorf("the namespace %s belongs to another donkey installation — pick a different space name (space: <name> in donkey.yaml)", namespace)
	}
	return fmt.Errorf("the namespace %s already exists and wasn't created by donkey — pick a different space name (space: <name> in donkey.yaml)", namespace)
}
