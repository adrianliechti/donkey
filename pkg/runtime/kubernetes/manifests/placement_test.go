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
// space. Names are derived and never parsed back — identity lives in labels —
// so hyphens on either side are fine, and two pairs may even spell the same
// name (the apply path refuses the second one by its labels).
func TestSharedNamesCarryTheSpace(t *testing.T) {
	p := shared()
	for _, tc := range []struct{ space, name, want string }{
		{"shop", "api", "shop-api"},
		{"my-shop", "api", "my-shop-api"},
		{"a", "b-c", "a-b-c"},
		{"a-b", "c", "a-b-c"},
	} {
		if got := p.Name(tc.space, tc.name); got != tc.want {
			t.Errorf("Name(%q, %q) = %q, want %q", tc.space, tc.name, got, tc.want)
		}
	}
	if p.Namespace("shop") != "donkey-apps" || p.Namespace("blog") != "donkey-apps" {
		t.Error("every space shares the one namespace")
	}
}

// Spaces follow the same DNS-label rule as apps and services; only donkey's
// own names are off limits.
func TestSpaceNamesAreDNSLabels(t *testing.T) {
	for _, p := range []Placement{shared(), perSpace()} {
		for _, space := range []string{"-a", "a-", "Team", "a.b", "", "donkey", "platform", "system"} {
			if err := p.ValidateNames(space, "api"); err == nil {
				t.Errorf("invalid space %q was accepted", space)
			}
		}
		for _, space := range []string{"team", "my-team", "team-2"} {
			for _, app := range []string{"api", "my-api", "api-v2"} {
				if err := p.ValidateNames(space, app); err != nil {
					t.Errorf("valid pair %q/%q rejected: %v", space, app, err)
				}
			}
		}
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
	if m.GetName() != "shop-space" || m.GetLabels()[LabelRole] != RoleSpace {
		t.Errorf("unexpected space settings identity: %s, %v", m.GetName(), m.GetLabels())
	}
	if m.GetName() == AppRouteName(shared().Name("shop", "space")) {
		t.Error("space config collides with an app route")
	}
}
