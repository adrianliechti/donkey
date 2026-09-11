package manifests

import (
	"fmt"
	"strings"

	"github.com/adrianliechti/donkey/internal/config"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// NameSeparator joins a space onto a name where one namespace holds several
// spaces. Object names are derived, never parsed: a space and an app may both
// contain hyphens, and whoever needs to know what an object is reads its
// donkey.local/* labels, the way Helm's <release>-<chart> names work.
const NameSeparator = "-"

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
//
// Two pairs can render the same name ("a-b"/"c" and "a"/"b-c" both come out
// as "a-b-c"). Uniqueness is settled where the object is applied: an existing
// object whose identity labels name another space or app is refused, never
// taken over.
func (p Placement) Name(space, name string) string {
	if p.Shared() {
		return space + NameSeparator + name
	}
	return name
}

// SpaceOf recovers the space from a namespace — the inverse of Namespace for
// the per-space arrangement, where the namespace name is the only record. In
// the shared arrangement the namespace says nothing, so callers read the
// donkey.local/space label instead.
func (p Placement) SpaceOf(namespace string) string {
	if p.Shared() {
		return ""
	}
	return strings.TrimPrefix(namespace, p.NamespacePrefix)
}

// SpaceMarkerName holds a space's settings in either placement mode, and
// records empty spaces when there is no namespace of their own.
func SpaceMarkerName(space string) string { return space + "-space" }

// SpaceMarker holds space settings. Labels identify the space independently
// of its name and namespace.
func SpaceMarker(space, namespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      SpaceMarkerName(space),
			Namespace: namespace,
			Labels:    SpaceLabels(space),
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

// ValidateNames checks the space name and the combined Service name length.
func (p Placement) ValidateNames(space, app string) error {
	if err := config.ValidateSpaceName(space); err != nil {
		return err
	}
	if !p.Shared() {
		return nil
	}
	if n := len(p.Name(space, app)); n > MaxObjectName {
		return fmt.Errorf("%q in space %q comes to %d characters as one object name (%q), and the cluster allows %d — shorten one of them",
			app, space, n, p.Name(space, app), MaxObjectName)
	}
	return nil
}
