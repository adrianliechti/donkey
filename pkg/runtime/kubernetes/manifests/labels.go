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
	// Deployment (JSON), so no-build deploys can keep them.
	AnnotationDecisions = "donkey.dev/decisions"
	// AnnotationConsole is separate from decisions: a deploy rewrites those
	// from donkey.yaml, while a console choice survives deploys — until
	// donkey.yaml settles the matter, which always wins.
	AnnotationConsole = "donkey.dev/console"
	// LabelApp / LabelComponent identify donkey-owned resources;
	// LabelSpace groups the apps of one space (one namespace holds them all).
	LabelApp       = "donkey.dev/app"
	LabelComponent = "donkey.dev/component"
	LabelSpace     = "donkey.dev/space"
	// LabelInstance names the installation an object belongs to — the control
	// plane's namespace. Several donkeys can share a cluster, and every
	// cluster-wide sweep donkey does would otherwise match the others' objects
	// too: managed-by=donkey says "a donkey wrote this", not "this one did".
	LabelInstance = "donkey.dev/instance"
	// LabelShared marks an app's binding Secret as a copy of a space-shared
	// addon's — removing the shared addon has to find (only) these copies.
	LabelShared = "donkey.dev/shared"

	ComponentWeb          = "web"
	ComponentRegistryAuth = "registry-auth"
)

// Labels marks one object as donkey's; callers that don't know the space pass
// "" and get no space label.
//
// Two sets say the same things. donkey.dev/* is what donkey selects on. The
// app.kubernetes.io/* set is the Kubernetes-recommended vocabulary, so the
// grouping is legible to k9s, Lens and Backstage, which group by `part-of`.
//
// Metadata only: the Deployment's selector is written separately from
// donkey.dev/* alone, because a selector is immutable once created.
func Labels(space, app, component string) map[string]string {
	l := map[string]string{
		"app.kubernetes.io/name":       app,
		"app.kubernetes.io/managed-by": ManagedByValue,
		LabelApp:                       app,
		LabelComponent:                 component,
	}
	if component != "" {
		l["app.kubernetes.io/component"] = component
	}
	if space != "" {
		l[LabelSpace] = space
		l["app.kubernetes.io/part-of"] = space
	}
	return l
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
