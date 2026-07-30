package manifests

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
)

func perSpace() Placement { return Placement{NamespacePrefix: "donkey-"} }
func shared() Placement   { return Placement{SharedNamespace: "donkey-apps"} }

// A namespace per space is the default, and it must keep naming things
// exactly as it always did — the plain name is what lets siblings reach each
// other as "api".
func TestPerSpaceNamesAreUnchanged(t *testing.T) {
	p := perSpace()
	if got := p.Namespace("shop"); got != "donkey-shop" {
		t.Errorf("namespace: got %q, want donkey-shop", got)
	}
	if got := p.Name("shop", "api"); got != "api" {
		t.Errorf("name: got %q, want api — qualifying it would break sibling DNS", got)
	}
	if got := p.SpaceOf("donkey-shop"); got != "shop" {
		t.Errorf("space: got %q, want shop", got)
	}
	if p.Shared() {
		t.Error("a namespace per space is not the shared arrangement")
	}
}

// One namespace holds one Service called "api", so every name carries its
// space — and comes back out again unchanged.
func TestSharedNamesRoundTrip(t *testing.T) {
	p := shared()
	for _, tc := range []struct{ space, name, want string }{
		{"shop", "api", "shop--api"},
		{"shop", "shared", "shop--shared"},
		{"a", "b-c", "a--b-c"},
		{"a-b", "c", "a-b--c"},
	} {
		got := p.Name(tc.space, tc.name)
		if got != tc.want {
			t.Errorf("Name(%q, %q) = %q, want %q", tc.space, tc.name, got, tc.want)
		}
		if back := p.Unname(tc.space, got); back != tc.name {
			t.Errorf("Unname(%q, %q) = %q, want %q", tc.space, got, back, tc.name)
		}
	}
	// The pair that makes the whole separator rule necessary: with a single
	// dash, ("a", "b-c") and ("a-b", "c") would both be "a-b-c" — one
	// Deployment for two apps, and the second deploy silently wins.
	if p.Name("a", "b-c") == p.Name("a-b", "c") {
		t.Fatal("two different apps must never render the same object name")
	}
	if p.Namespace("shop") != "donkey-apps" || p.Namespace("blog") != "donkey-apps" {
		t.Error("every space shares the one namespace")
	}
}

// The separator only stays unambiguous if neither half may contain it.
func TestSharedRejectsSeparatorInNames(t *testing.T) {
	p := shared()
	if err := p.ValidateNames("a--b", "c"); err == nil {
		t.Error("a space containing the separator must be refused")
	}
	if err := p.ValidateNames("a", "b--c"); err == nil {
		t.Error("an app containing the separator must be refused")
	}
	if err := p.ValidateNames("shop", "api"); err != nil {
		t.Errorf("ordinary names must pass: %v", err)
	}
	// A namespace per space joins nothing, so it constrains nothing.
	if err := perSpace().ValidateNames("a--b", "c--d"); err != nil {
		t.Errorf("per-space placement has no separator to protect: %v", err)
	}
}

// Both names fit on their own; joined they exceed what a Service name may be,
// and only the pair can say so.
func TestSharedRejectsOverlongPair(t *testing.T) {
	long := strings.Repeat("a", 40)
	if err := perSpace().ValidateNames(long, long); err != nil {
		t.Errorf("per-space names are not joined, so length is not a pair problem: %v", err)
	}
	err := shared().ValidateNames(long, long)
	if err == nil {
		t.Fatal("81 characters of object name must be refused, not sent to the cluster")
	}
	if !strings.Contains(err.Error(), "63") {
		t.Errorf("the error should say what the limit is: %v", err)
	}
}

// A space with no namespace of its own still has to exist — a marker records
// it, and it can never be mistaken for an app's object.
func TestSpaceObject(t *testing.T) {
	obj := perSpace().SpaceObject("shop")
	if _, ok := obj.(*corev1.Namespace); !ok {
		t.Fatalf("per-space placement makes a Namespace, got %T", obj)
	}

	obj = shared().SpaceObject("shop")
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		t.Fatalf("shared placement makes a marker ConfigMap, got %T", obj)
	}
	m, err := meta.Accessor(cm)
	if err != nil {
		t.Fatal(err)
	}
	if m.GetNamespace() != "donkey-apps" {
		t.Errorf("the marker belongs in the shared namespace, got %q", m.GetNamespace())
	}
	if m.GetLabels()[LabelSpace] != "shop" {
		t.Error("the marker must carry its space, or an empty space cannot be listed")
	}
	// App names cannot contain a dot, so no app's object can ever be named
	// like a marker — including an app called "space".
	if !strings.Contains(m.GetName(), ".") {
		t.Errorf("the marker name %q must be unreachable for an app name", m.GetName())
	}
	if !IsSpaceMarker(m.GetName()) {
		t.Errorf("%q should be recognised as a marker", m.GetName())
	}
	if IsSpaceMarker(shared().Name("shop", "space")) {
		t.Error("an app called \"space\" must not look like a space marker")
	}
}
