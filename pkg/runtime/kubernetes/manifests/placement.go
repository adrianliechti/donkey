package manifests

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// NameSeparator joins a space and a name into one object name when a whole
// installation shares a single namespace. Space and app names may not contain
// it (ValidateNames refuses them), so the join is unambiguous: without that
// rule the pair ("a", "b-c") and the pair ("a-b", "c") would both render
// "a-b-c" and the second deploy would silently overwrite the first.
const NameSeparator = "--"

// spaceMarkerPrefix names the object that stands in for a space when there is
// no namespace to be the space. It contains dots, which no space or app name
// may — so a marker can never collide with an app's objects.
const spaceMarkerPrefix = "donkey.space."

// ComponentSpace marks the space marker object.
const ComponentSpace = "space"

// Placement decides where a space's objects live and what they are called.
//
// Two arrangements, one type. By default every space gets its own namespace
// (<prefix><space>) and objects keep their plain names, which is what makes an
// app's Service reachable as "api" from its siblings. When SharedNamespace is
// set, every app of the installation lives in that one namespace — donkey
// neither creates nor deletes it — and object names carry the space, because a
// namespace holds one "api" and no more.
type Placement struct {
	// SharedNamespace, when set, is the single namespace every app lives in.
	// It belongs to whoever created it, not to donkey.
	SharedNamespace string
	// NamespacePrefix maps a space onto its own namespace; only consulted
	// when SharedNamespace is empty.
	NamespacePrefix string
}

// Shared reports whether one namespace holds the whole installation.
func (p Placement) Shared() bool { return p.SharedNamespace != "" }

// Namespace is where the space's objects live.
func (p Placement) Namespace(space string) string {
	if p.Shared() {
		return p.SharedNamespace
	}
	return p.NamespacePrefix + space
}

// Name is the object name for one of the space's things — an app, an addon,
// a secret prefix. Unqualified when the space owns its namespace, because
// that is what lets siblings reach each other by plain name.
func (p Placement) Name(space, name string) string {
	if p.Shared() {
		return space + NameSeparator + name
	}
	return name
}

// Unname recovers the plain name from an object name. The inverse of Name.
func (p Placement) Unname(space, objectName string) string {
	if p.Shared() {
		return strings.TrimPrefix(objectName, space+NameSeparator)
	}
	return objectName
}

// SpaceOf recovers the space from a namespace — the inverse of Namespace for
// the per-space arrangement, where the namespace name is the only record. In
// the shared arrangement the namespace says nothing, so callers read the
// donkey.dev/space label instead.
func (p Placement) SpaceOf(namespace string) string {
	if p.Shared() {
		return ""
	}
	return strings.TrimPrefix(namespace, p.NamespacePrefix)
}

// SpaceMarkerName is the object that records an empty space. A space with
// apps is evident from its apps; one holding only services — or nothing at
// all, freshly created in the console — needs something to exist.
func SpaceMarkerName(space string) string { return spaceMarkerPrefix + space }

// IsSpaceMarker reports whether an object name is a space marker.
func IsSpaceMarker(name string) bool { return strings.HasPrefix(name, spaceMarkerPrefix) }

// SpaceMarker records a space that has no namespace of its own. It carries
// the space label like everything else donkey writes, so the same sweep that
// finds a space's apps finds the space itself.
func SpaceMarker(space, namespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      SpaceMarkerName(space),
			Namespace: namespace,
			Labels:    Labels(space, SpaceMarkerName(space), ComponentSpace),
		},
	}
}

// SpaceObject is what has to exist for a space to exist: its namespace when
// donkey owns one, and otherwise a marker in the shared namespace. Every path
// that creates something in a space applies this first, so a space is never
// half-there.
func (p Placement) SpaceObject(space string) runtime.Object {
	return SpaceHome(space, p.Namespace(space), p.Shared())
}

// MaxObjectName is the length a Service name may have: it becomes a DNS
// label, and the cluster rejects anything longer. Deployments and Secrets
// allow more, but an app needs all of them, so the tightest one is the rule.
const MaxObjectName = 63

// ValidateNames checks what the shared arrangement adds to donkey's ordinary
// name rules: the two names are joined, so together they have to fit, and
// neither may contain the separator that joins them.
func (p Placement) ValidateNames(space, app string) error {
	if !p.Shared() {
		return nil
	}
	for _, n := range []struct{ what, value string }{{"space", space}, {"name", app}} {
		if strings.Contains(n.value, NameSeparator) {
			return fmt.Errorf("this installation puts every app in one namespace, so the %s %q may not contain %q — it is what separates the space from the app in an object's name", n.what, n.value, NameSeparator)
		}
	}
	if n := len(p.Name(space, app)); n > MaxObjectName {
		return fmt.Errorf("%q in space %q comes to %d characters as one object name (%q), and the cluster allows %d — shorten one of them",
			app, space, n, p.Name(space, app), MaxObjectName)
	}
	return nil
}
