package kubernetes

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// Objects donkey applied before it stamped an installation onto them carry no
// donkey.dev/instance at all, and `ours` reads a missing label as "not this
// installation's". That is the right reading of an object some other writer
// made, and the wrong one for an object donkey itself wrote before the label
// existed — with the whole previous version as the cost: its spaces and apps
// vanish from every listing, the gateway drops their routes, donkey-ai stops
// recognising their AI tokens, and a redeploy is refused with "belongs to
// another donkey installation".
//
// So an upgrade claims them, once, from the installer. Explicitly, rather
// than by softening `ours`: unstamped would then mean "anybody's" forever,
// and two installations sharing a prefix would each keep claiming what the
// other made — the exact thing the label exists to prevent. Claiming at
// install time happens with a person watching it, says what it did, and
// finds nothing on the second run.

// ClaimResult is what one sweep did, for the installer to report.
type ClaimResult struct {
	// Namespaces claimed, by name.
	Namespaces []string
	// Objects claimed inside them.
	Objects int
	// Outside are unstamped space namespaces this installation would never
	// name — what a namespacePrefix changed during the upgrade leaves
	// stranded. Silence is the worst way to find that out.
	Outside []string
}

// ClaimUnstamped takes over the resources of an installation that predates
// instance labelling: this installation's space namespaces, and everything
// donkey put inside them.
//
// The caller decides that this is an upgrade of this installation rather than
// a new one standing up beside an old one — on a cluster where both are true
// of the same prefix, only the installation that owns the control plane may
// claim.
func (c *Client) ClaimUnstamped(ctx context.Context, p manifests.Placement) (ClaimResult, error) {
	var res ClaimResult
	// One namespace for the whole installation is younger than the label, so
	// nothing in that arrangement can predate it. It is also the arrangement
	// where an unstamped object could not be attributed at all: a namespace
	// donkey was given may hold somebody else's apps beside donkey's.
	if p.Shared() {
		return res, nil
	}
	// The space label is what makes a namespace a space — the control plane's
	// own never carries it. Reading through the same client that writes is
	// what makes "claimed once, then nothing left" true.
	namespaces, err := c.Dynamic.Resource(gvrFor["Namespace"]).Namespace("").List(ctx, metav1.ListOptions{
		LabelSelector: manifests.LabelSpace,
	})
	if err != nil {
		return res, err
	}
	for _, ns := range namespaces.Items {
		name := ns.GetName()
		// Only namespaces donkey made, and only ones it has not stamped: a
		// label naming another installation is that installation's answer,
		// and claiming must never overwrite one.
		if ns.GetAnnotations()[manifests.AnnotationManaged] != "true" || ns.GetDeletionTimestamp() != nil {
			continue
		}
		if _, stamped := ns.GetLabels()[manifests.LabelInstance]; stamped {
			continue
		}
		if !strings.HasPrefix(name, p.NamespacePrefix) {
			res.Outside = append(res.Outside, name)
			continue
		}
		if err := c.claim(ctx, gvrFor["Namespace"], "", name); err != nil {
			return res, fmt.Errorf("claiming the namespace %s: %w", name, err)
		}
		res.Namespaces = append(res.Namespaces, name)
		n, err := c.claimContents(ctx, name)
		if err != nil {
			return res, err
		}
		res.Objects += n
	}
	return res, nil
}

// claimContents stamps what donkey put in one space namespace. The kinds are
// the ones the delete sweeps use — everything an app or a space service is
// made of — so nothing an app consists of is left behind: the Deployment the
// console lists it by, the ConfigMap the gateway routes it through, the
// Secret donkey-ai checks its AI token against.
func (c *Client) claimContents(ctx context.Context, namespace string) (int, error) {
	var n int
	for _, kind := range appKinds {
		list, err := c.Dynamic.Resource(gvrFor[kind]).Namespace(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/managed-by=" + manifests.ManagedByValue,
		})
		if err != nil {
			return n, fmt.Errorf("looking at the %s objects of %s: %w", kind, namespace, err)
		}
		for _, obj := range list.Items {
			if _, stamped := obj.GetLabels()[manifests.LabelInstance]; stamped {
				continue
			}
			if err := c.claim(ctx, gvrFor[kind], namespace, obj.GetName()); err != nil {
				return n, fmt.Errorf("claiming the %s %s: %w", kind, obj.GetName(), err)
			}
			n++
		}
	}
	return n, nil
}

// claim adds the instance label and nothing else. A merge patch rather than
// an apply: server-side apply would take over the object's labels as a whole
// — and on a namespace stamp the managed annotation that makes it deletable —
// where this has to touch one key and leave the rest as it found it.
//
// An object that vanishes mid-sweep is not an error: a deploy or a destroy
// running alongside the upgrade is entitled to remove things.
func (c *Client) claim(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) error {
	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, manifests.LabelInstance, c.instance())
	_, err := c.Dynamic.Resource(gvr).Namespace(namespace).Patch(
		ctx, name, types.MergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: fieldManager})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
