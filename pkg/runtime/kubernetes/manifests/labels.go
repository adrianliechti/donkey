package manifests

import (
	"regexp"
	"strings"
)

const (
	// ManagedByValue marks every object donkey owns.
	ManagedByValue = "donkey"
	// AnnotationManaged marks namespaces donkey created (and may delete).
	AnnotationManaged = "donkey.dev/managed"
	// AnnotationImage records the deployed image on the Deployment.
	AnnotationImage = "donkey.dev/image"
	// AnnotationDecisions records deploy-time analysis decisions on the
	// Deployment (JSON), so no-build deploys can keep them. It is read back
	// through the provider's meta API — keep it equal to runtime.MetaDecisions.
	AnnotationDecisions = "donkey.dev/decisions"
	// AnnotationConsole is separate from decisions: a deploy rewrites those
	// from donkey.yaml, while a console choice survives deploys — until
	// donkey.yaml settles the matter, which always wins. It is written and
	// read only through the provider's meta API — keep it equal to
	// runtime.MetaConsole.
	AnnotationConsole = "donkey.dev/console"
	// LabelSpace and LabelApp are logical donkey identities. Object names may
	// be qualified when several spaces share a namespace; labels never are.
	LabelApp   = "donkey.dev/app"
	LabelAddon = "donkey.dev/addon"
	LabelSpace = "donkey.dev/space"
	// Kubernetes already defines the component vocabulary. Using its key for
	// both display and selectors avoids maintaining a duplicate
	// donkey.dev/component label with the same value on every object.
	LabelComponent = "app.kubernetes.io/component"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// LabelInstallation names the installation an object belongs to — the control
	// plane's namespace. Several donkeys can share a cluster, and every
	// cluster-wide sweep donkey does would otherwise match the others' objects
	// too: managed-by=donkey says "a donkey wrote this", not "this one did".
	LabelInstallation = "donkey.dev/installation"

	ComponentWeb          = "web"
	ComponentAddon        = "addon"
	ComponentBinding      = "binding"
	ComponentConfig       = "config"
	ComponentStorage      = "storage"
	ComponentRoute        = "route"
	ComponentPathRoute    = "path-route"
	ComponentEdge         = "edge"
	ComponentSpace        = "space"
	ComponentRegistryAuth = "registry-auth"
	// ComponentFiles is the short-lived file-browser helper pod. It carries
	// the app's own space/app labels, so it is one of the app's objects.
	ComponentFiles = "files"
)

// Labels marks one object as donkey's; callers that don't know the space pass
// "" and get no space label.
//
// Donkey-specific identity uses donkey.dev/*; the component and descriptive
// grouping use Kubernetes' recommended app.kubernetes.io/* vocabulary so the
// same objects are legible to k9s, Lens and Backstage.
//
// Metadata only: the Deployment's selector is written separately from
// these richer labels, because a selector is immutable once created.
func Labels(space, app, component string) map[string]string {
	l := baseLabels(space, app, component)
	if app != "" {
		l[LabelApp] = app
	}
	return l
}

// AddonLabels identifies a space service independently of the app bindings
// that consume it. Shared services deliberately have no pseudo-app label.
func AddonLabels(space, addon, component string) map[string]string {
	l := baseLabels(space, addon, component)
	if addon != "" {
		l[LabelAddon] = addon
	}
	return l
}

// SpaceLabels marks the namespace or marker that represents a logical space.
func SpaceLabels(space string) map[string]string {
	return baseLabels(space, space, ComponentSpace)
}

// SystemLabels marks one control-plane component. System workloads have no
// tenant app or space identity.
func SystemLabels(name string) map[string]string {
	return baseLabels("", name, name)
}

func baseLabels(space, name, component string) map[string]string {
	l := map[string]string{LabelManagedBy: ManagedByValue}
	if name != "" {
		l["app.kubernetes.io/name"] = name
	}
	if component != "" {
		l[LabelComponent] = component
	}
	if space != "" {
		l[LabelSpace] = space
		l["app.kubernetes.io/part-of"] = space
	}
	return l
}

// AppSelector is the immutable identity shared by an app Deployment and
// Service. Installation ownership is added at the apply/render boundary.
func AppSelector(space, app, component string) map[string]string {
	return map[string]string{LabelSpace: space, LabelApp: app, LabelComponent: component}
}

// AddonSelector is the immutable identity shared by an addon Deployment and
// Service. Installation ownership is added at the apply/render boundary.
func AddonSelector(space, addon string) map[string]string {
	return map[string]string{LabelSpace: space, LabelAddon: addon, LabelComponent: ComponentAddon}
}

// SystemSelector is the small, stable selector for one control-plane
// workload. Descriptive name, part-of and version labels stay out of selectors.
func SystemSelector(name string) map[string]string {
	return map[string]string{LabelComponent: name}
}

// labelValue matches what Kubernetes accepts as a label value.
var labelValue = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)

// withVersion labels from the image tag. The tag is not donkey's to choose,
// and a value the API server rejects would fail the whole apply — so a tag
// that cannot be a label is dropped. A missing label costs a line in k9s; a
// rejected one costs the deploy.
func withVersion(l map[string]string, imageRef string) map[string]string {
	tag := imageRef
	if i := strings.LastIndex(tag, "@"); i >= 0 {
		tag = tag[:i] // a digest is not a version
	}
	slash := strings.LastIndex(tag, "/")
	if i := strings.LastIndex(tag, ":"); i > slash {
		tag = tag[i+1:]
	} else {
		return l
	}
	if len(tag) > 63 || !labelValue.MatchString(tag) {
		return l
	}
	l["app.kubernetes.io/version"] = tag
	return l
}
