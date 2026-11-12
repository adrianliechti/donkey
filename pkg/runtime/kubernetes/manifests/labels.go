package manifests

import (
	"regexp"
	"strings"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
)

const (
	// ManagedByValue marks every object donkey owns. It doubles as the
	// platform's own part-of value: every control-plane object is part of
	// the application called "donkey".
	ManagedByValue = "donkey"
	// AnnotationManaged marks namespaces donkey created (and may delete).
	AnnotationManaged = "donkey.local/managed"
	// AnnotationImage records the deployed image on the Deployment.
	AnnotationImage = "donkey.local/image"
	// AnnotationStack records what the app is written in (detect.Stack), so
	// lists can show it without reading the app's history.
	AnnotationStack = "donkey.local/stack"
	// AnnotationSecurity records the vulnerability scan's outcome for the
	// deployed image (JSON), so lists can show it without opening the
	// release record.
	AnnotationSecurity = "donkey.local/security"
	// AnnotationDecisions records deploy-time analysis decisions on the
	// Deployment (JSON), so no-build deploys can keep them. It is read back
	// through the provider's meta API, so it is spelled by that API.
	AnnotationDecisions = rt.MetaDecisions
	// AnnotationConsole is separate from decisions: a deploy rewrites those
	// from donkey.yaml, while a console choice survives deploys — until
	// donkey.yaml settles the matter, which always wins. It is written and
	// read only through the provider's meta API, so it is spelled by that API.
	AnnotationConsole = rt.MetaConsole
	// AnnotationConfigHash rolls control-plane pods that read their
	// configuration only at startup; each deployment hashes what it consumes.
	AnnotationConfigHash = "donkey.local/config-hash"
	// AnnotationRestartedAt is patched into an app's pod template to force a
	// restart, the way `kubectl rollout restart` does.
	AnnotationRestartedAt = "donkey.local/restarted-at"
	// AnnotationFilesExpires is the deadline after which an abandoned
	// file-browser helper pod may be reaped.
	AnnotationFilesExpires = "donkey.local/files-expires-at"
	// LabelSpace and LabelApp are logical donkey identities. Object names may
	// be qualified when several spaces share a namespace; these labels and the
	// standard instance label never are.
	LabelApp   = "donkey.local/app"
	LabelAddon = "donkey.local/addon"
	LabelSpace = "donkey.local/space"
	// Role is donkey's internal, immutable classification. Ownership
	// checks, sweeps and selectors use only donkey.local/* labels; Kubernetes'
	// recommended app.kubernetes.io/* labels below are descriptive metadata and
	// never grant authority over an object.
	LabelRole      = "donkey.local/role"
	LabelName      = "app.kubernetes.io/name"
	LabelInstance  = "app.kubernetes.io/instance"
	LabelVersion   = "app.kubernetes.io/version"
	LabelPartOf    = "app.kubernetes.io/part-of"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// LabelInstallation names the installation an object belongs to — the control
	// plane's namespace. Several donkeys can share a cluster, and every
	// cluster-wide sweep donkey does would otherwise match the others' objects
	// too: managed-by=donkey says "a donkey wrote this", not "this one did".
	LabelInstallation = "donkey.local/installation"
	// LabelNamespaceName is the well-known label the API server keeps equal to
	// a namespace's name, used to pin NetworkPolicy peers to one namespace.
	LabelNamespaceName = "kubernetes.io/metadata.name"

	RoleWorkload     = "workload"
	RoleAddon        = "addon"
	RoleBinding      = "binding"
	RoleConfig       = "config"
	RoleStorage      = "storage"
	RoleRoute        = "route"
	RolePathRoute    = "path-route"
	RoleEdge         = "edge"
	RoleSpace        = "space"
	RoleRegistryAuth = "registry-auth"
	// RoleFiles is the short-lived file-browser helper pod. It carries
	// the app's own space/app labels, so it is one of the app's objects.
	RoleFiles = "files"
)

// Labels marks one object as donkey's; callers that don't know the space pass
// "" and get no space or instance label.
//
// Donkey-specific identity uses donkey.local/*. Kubernetes' recommended
// app.kubernetes.io/* vocabulary is metadata only:
// name is the logical app, instance and part-of are the space, and managed-by
// names donkey. app.kubernetes.io/component is deliberately omitted because
// donkey does not know an app's architectural role.
//
// Metadata only: the Deployment's selector is written separately from
// these richer labels, because a selector is immutable once created.
func Labels(space, app, role string) map[string]string {
	l := baseLabels(space, app, role)
	if app != "" {
		l[LabelApp] = app
	}
	return l
}

// AddonLabels identifies a space service independently of the app bindings
// that consume it. Shared services deliberately have no pseudo-app label.
func AddonLabels(space, addon, role string) map[string]string {
	l := baseLabels(space, addon, role)
	if addon != "" {
		l[LabelAddon] = addon
	}
	return l
}

// SpaceLabels marks the namespace or marker that represents a logical space.
// The space is its own instance: it is the unit, not a copy of one.
func SpaceLabels(space string) map[string]string {
	l := baseLabels(space, space, RoleSpace)
	l[LabelInstance] = space
	return l
}

// SystemLabels marks one control-plane workload. System workloads have no
// tenant app or space identity; they are part of the platform itself, and
// their instance — the installation — is stamped at the apply/render
// boundary, where it is known (see StampInstallation).
func SystemLabels(name string) map[string]string {
	l := baseLabels("", name, SystemRole(name))
	l[LabelPartOf] = ManagedByValue
	return l
}

// SystemRole removes object-name plumbing from a control-plane role. This
// keeps Kubernetes and Docker aligned: donkey-server is named donkey-server
// but has role=server in either runtime.
func SystemRole(name string) string { return strings.TrimPrefix(name, "donkey-") }

func baseLabels(space, name, role string) map[string]string {
	l := map[string]string{LabelManagedBy: ManagedByValue}
	if name != "" {
		l[LabelName] = name
	}
	if role != "" {
		l[LabelRole] = role
	}
	if space != "" {
		l[LabelSpace] = space
		l[LabelPartOf] = space
		// A space is donkey's release/lifecycle instance. The name + instance
		// pair identifies one app or addon without leaking shared-namespace
		// object-name qualification into descriptive metadata.
		l[LabelInstance] = space
	}
	return l
}

// AppSelector is the immutable identity shared by an app Deployment and
// Service. Installation ownership is added at the apply/render boundary.
func AppSelector(space, app, role string) map[string]string {
	return map[string]string{LabelSpace: space, LabelApp: app, LabelRole: role}
}

// AddonSelector is the immutable identity shared by an addon Deployment and
// Service. Installation ownership is added at the apply/render boundary.
func AddonSelector(space, addon string) map[string]string {
	return map[string]string{LabelSpace: space, LabelAddon: addon, LabelRole: RoleAddon}
}

// SystemSelector is the small, stable selector for one control-plane
// workload. Descriptive name, instance, part-of and version labels stay out
// of selectors.
func SystemSelector(name string) map[string]string {
	return map[string]string{LabelRole: SystemRole(name)}
}

// SystemPodSelector is the NetworkPolicy-peer form of SystemSelector: one
// control-plane workload's pods, bounded to one installation so a second
// donkey's same-named pods never match.
func SystemPodSelector(installation, name string) map[string]string {
	sel := SystemSelector(name)
	sel[LabelInstallation] = installation
	return sel
}

// SpacePodSelector selects every pod of one space that one installation owns
// — the peer both halves of the space isolation fence are built from.
func SpacePodSelector(installation, space string) map[string]string {
	return map[string]string{LabelInstallation: installation, LabelSpace: space}
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
	l[LabelVersion] = tag
	return l
}
