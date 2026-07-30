package kubernetes

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// spaceNS is a space's namespace as the version before instance labelling
// left it: donkey's managed annotation and the space label, no instance.
func spaceNS(name, space, instance string) *corev1.Namespace {
	ns := nsWith(name, true, instance)
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	ns.Labels[manifests.LabelSpace] = space
	return ns
}

// unmanaged drops the annotation that says donkey made the namespace: a space
// label on somebody's own namespace must not turn it into donkey's.
func unmanaged(ns *corev1.Namespace) *corev1.Namespace {
	ns.Annotations = nil
	return ns
}

func donkeyLabels(space, app string) map[string]string {
	return manifests.Labels(space, app, manifests.ComponentWeb)
}

// An upgrade has to find the apps of the version before it. Objects donkey
// wrote before instance labelling carry no label at all — those are this
// installation's own and claiming them is what keeps its spaces visible.
// Everything else stays as it was: another installation's stamp, a namespace
// donkey never made, a namespace outside this installation's prefix, and an
// object in a claimed namespace that is nobody's business but its owner's.
func TestClaimUnstamped(t *testing.T) {
	ctx := context.Background()
	stamped := donkeyLabels("x", "api")
	stamped[manifests.LabelInstance] = "donkey-system"
	dyn := dynamicfake.NewSimpleDynamicClient(scheme.Scheme,
		spaceNS("donkey-x", "x", ""),
		spaceNS("donkey-y", "y", "bank-donkey-system"),
		spaceNS("legacy-w", "w", ""),
		unmanaged(spaceNS("donkey-z", "z", "")),

		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: "shop", Namespace: "donkey-x", Labels: donkeyLabels("x", "shop")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: "shop-db", Namespace: "donkey-x", Labels: donkeyLabels("x", "shop")}},
		// Already this installation's: claiming must not count it again.
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: "api", Namespace: "donkey-x", Labels: stamped}},
		// No managed-by: donkey did not write it and must not touch it.
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{
			Name: "somebody-elses", Namespace: "donkey-x"}},
	)

	c := (&Client{Dynamic: dyn}).ForInstance("")
	res, err := c.ClaimUnstamped(ctx, manifests.Placement{NamespacePrefix: "donkey-"})
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Namespaces) != 1 || res.Namespaces[0] != "donkey-x" {
		t.Fatalf("claimed namespaces = %v, want [donkey-x]", res.Namespaces)
	}
	if res.Objects != 2 {
		t.Errorf("claimed objects = %d, want 2 (the unstamped deployment and secret)", res.Objects)
	}
	if len(res.Outside) != 1 || res.Outside[0] != "legacy-w" {
		t.Errorf("outside the prefix = %v, want [legacy-w]", res.Outside)
	}

	instanceOf := func(name string) string {
		ns, err := dyn.Resource(gvrFor["Namespace"]).Namespace("").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return ns.GetLabels()[manifests.LabelInstance]
	}
	if got := instanceOf("donkey-x"); got != "donkey-system" {
		t.Errorf("donkey-x instance = %q, want donkey-system", got)
	}
	if got := instanceOf("donkey-y"); got != "bank-donkey-system" {
		t.Errorf("another installation's namespace was rewritten to %q", got)
	}
	for _, name := range []string{"legacy-w", "donkey-z"} {
		if got := instanceOf(name); got != "" {
			t.Errorf("%s was claimed (%q) — it is not this installation's to take", name, got)
		}
	}

	// The space label alone says nothing about who wrote an object inside.
	svc, err := dyn.Resource(gvrFor["Service"]).Namespace("donkey-x").Get(ctx, "somebody-elses", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, stamped := svc.GetLabels()[manifests.LabelInstance]; stamped {
		t.Error("an object donkey never wrote was claimed")
	}

	// Second run: everything is stamped, so there is nothing left to take.
	again, err := c.ClaimUnstamped(ctx, manifests.Placement{NamespacePrefix: "donkey-"})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Namespaces) != 0 || again.Objects != 0 {
		t.Errorf("a second install claimed %v/%d, want nothing", again.Namespaces, again.Objects)
	}
}

// One namespace for the whole installation is younger than the label, so
// nothing there can predate it — and an unstamped object in a namespace
// donkey was given may not be donkey's at all.
func TestClaimUnstampedSkipsSharedNamespace(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(scheme.Scheme, spaceNS("donkey-x", "x", ""))
	c := (&Client{Dynamic: dyn}).ForInstance("")
	res, err := c.ClaimUnstamped(context.Background(), manifests.Placement{SharedNamespace: "apps"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Namespaces) != 0 {
		t.Fatalf("claimed %v in the shared arrangement", res.Namespaces)
	}
}
