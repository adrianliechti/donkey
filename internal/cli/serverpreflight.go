package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/ui"
	k8s "github.com/adrianliechti/donkey/pkg/runtime/kubernetes"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// This file is everything the installer has to settle about namespaces before
// it applies anything: whether the ones it was given exist, whether their Pod
// Security Standard lets donkey's pods run at all, and whether donkey should
// be creating them in the first place.
//
// All of it is checked up front on purpose. A namespace that enforces the
// wrong policy does not fail the install — it fails admission later, when a
// Deployment quietly produces no pods, and nothing in donkey's own logs says
// why.

const psaEnforce = "pod-security.kubernetes.io/enforce"

// claimPrevious takes over the resources of an installation made before
// donkey stamped its own name on them, so an upgrade keeps sight of its
// spaces. It runs before anything is applied: from here on the cluster is
// read through the instance label, and what carries none is invisible.
func claimPrevious(ctx context.Context, u *ui.UI, client *k8s.Client, k config.InstallKubernetes) error {
	res, err := client.ClaimUnstamped(ctx, manifests.Placement{
		SharedNamespace: k.Workload.Namespace,
		NamespacePrefix: k.NamespacePrefix(),
	})
	if err != nil {
		return fmt.Errorf("couldn't take over the previous installation's resources: %w", err)
	}
	if len(res.Namespaces) > 0 {
		u.Info("Adopted %d space namespace(s) and %d objects from an installation that predates instance labelling: %s.",
			len(res.Namespaces), res.Objects, strings.Join(res.Namespaces, ", "))
	}
	for _, ns := range res.Outside {
		u.Warn("The namespace %s is a donkey space from before instance labelling, but it does not carry this installation's prefix %q — so it was left alone, and the apps in it stay invisible here. If it is this installation's (a namespacePrefix that changed with the upgrade), claim it: kubectl label namespace %s %s=%s",
			ns, k.NamespacePrefix(), ns, manifests.LabelInstance, k.SystemNamespace())
	}
	return nil
}

// namespacePlan is what the installer decided about the two namespaces.
type namespacePlan struct {
	// SkipSystemNamespace leaves the control plane's namespace out of the
	// apply, because it already exists and is not donkey's to adopt.
	SkipSystemNamespace bool
}

// planNamespaces checks the namespaces this installation will use and returns
// what the render needs to know. It refuses rather than warns wherever
// continuing would produce an installation that cannot work.
func planNamespaces(ctx context.Context, u *ui.UI, client *k8s.Client, k config.InstallKubernetes) (namespacePlan, error) {
	var plan namespacePlan
	system := k.SystemNamespace()

	labels, exists, err := client.NamespaceLabels(ctx, system)
	if err != nil {
		return plan, fmt.Errorf("couldn't look at the namespace %s: %w", system, err)
	}
	switch {
	case exists:
		// Never re-apply a namespace that is already there. Server-side apply
		// would take ownership of its labels and stamp donkey's managed
		// annotation on it — which is what makes a namespace deletable, and
		// this one may well belong to the platform team.
		plan.SkipSystemNamespace = true
		if err := checkPrivilegedAllowed(system, labels); err != nil {
			return plan, err
		}
	case k.System.Namespace != "":
		// Named explicitly and absent: on a cluster where namespaces are
		// handed out rather than created, saying "I'll make it" is exactly
		// the wrong guess.
		return plan, fmt.Errorf("the namespace %s doesn't exist — kubernetes.system.namespace names a namespace donkey expects to be given, so create it first (kubectl create namespace %s) or leave the setting empty to let donkey make its own", system, system)
	}

	workload := k.Workload.Namespace
	if workload == "" {
		return plan, nil
	}
	labels, exists, err = client.NamespaceLabels(ctx, workload)
	if err != nil {
		return plan, fmt.Errorf("couldn't look at the namespace %s: %w", workload, err)
	}
	if !exists {
		return plan, fmt.Errorf("the namespace %s doesn't exist — kubernetes.workload.namespace names a namespace donkey expects to be given (it never creates or deletes it), so create it first: kubectl create namespace %s", workload, workload)
	}
	return plan, checkAppsAllowed(u, workload, labels)
}

// checkPrivilegedAllowed refuses a control-plane namespace whose policy would
// reject the build engine. BuildKit runs privileged — it is how it builds
// images — so a namespace at "baseline" or "restricted" admits donkey-server
// and the gateway and then silently refuses the one pod that does the work.
func checkPrivilegedAllowed(namespace string, labels map[string]string) error {
	switch labels[psaEnforce] {
	case "", "privileged":
		return nil
	}
	return fmt.Errorf(`the namespace %s enforces the %q Pod Security Standard, which forbids privileged pods — donkey's build engine is one, so no app could ever be built there.

Either allow it on that namespace:
  kubectl label --overwrite namespace %s %s=privileged
or give donkey a namespace of its own by leaving kubernetes.system.namespace empty`,
		namespace, labels[psaEnforce], namespace, psaEnforce)
}

// checkAppsAllowed refuses an app namespace at "restricted". donkey enforces
// "baseline" on the namespaces it creates for a reason: a great many real
// images want to run as root, and restricted forbids it — every one of them
// would fail admission with nothing in the deploy log to explain it.
func checkAppsAllowed(u *ui.UI, namespace string, labels map[string]string) error {
	switch labels[psaEnforce] {
	case "restricted":
		return fmt.Errorf(`the namespace %s enforces the "restricted" Pod Security Standard, which forbids containers running as root — most real application images do, so deploys would fail admission rather than fail visibly.

donkey enforces "baseline" on namespaces it creates itself. To match it:
  kubectl label --overwrite namespace %s %s=baseline`, namespace, namespace, psaEnforce)
	case "", "privileged":
		u.Warn("The namespace %s enforces no Pod Security Standard. donkey sets %s=baseline on namespaces it creates, which is what keeps an app pod from asking for privileges; a namespace it was given keeps whatever policy you gave it.", namespace, psaEnforce)
	}
	return nil
}

// checkOptionalCRDs refuses a configuration that names machinery the cluster
// doesn't serve — without this, the apply fails with a bare "no matches for
// kind".
func checkOptionalCRDs(ctx context.Context, client *k8s.Client, settings *config.InstallFile) error {
	if settings.Gateway.HTTPRoute.Enabled() {
		ok, err := client.HasResource("gateway.networking.k8s.io/v1", "HTTPRoute")
		if err != nil {
			return fmt.Errorf("couldn't ask the cluster about Gateway API support: %w", err)
		}
		if !ok {
			return fmt.Errorf("gateway.httpRoute is set, but the cluster doesn't serve HTTPRoute (gateway.networking.k8s.io/v1) — install a Gateway API implementation first, or drop the setting")
		}
	}
	if settings.Gateway.AppGateway.Enabled() {
		for _, required := range []struct {
			groupVersion string
			kind         string
			setting      string
		}{
			{groupVersion: "gateway.networking.k8s.io/v1", kind: "Gateway", setting: "a Gateway API implementation"},
			{groupVersion: "gateway.networking.k8s.io/v1", kind: "HTTPRoute", setting: "a Gateway API implementation"},
			{groupVersion: "cert-manager.io/v1", kind: "Certificate", setting: "cert-manager"},
		} {
			ok, err := client.HasResource(required.groupVersion, required.kind)
			if err != nil {
				return fmt.Errorf("couldn't ask the cluster about %s support: %w", required.kind, err)
			}
			if !ok {
				return fmt.Errorf("gateway.appGateway is set, but the cluster doesn't serve %s (%s) — install %s first, or drop the setting", required.kind, required.groupVersion, required.setting)
			}
		}
	}
	return nil
}
