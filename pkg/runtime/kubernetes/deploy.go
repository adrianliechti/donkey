package kubernetes

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// Deploy applies the app's objects with the given image and waits for the
// rollout (unless spec.NoWait). Everything the Deployment needs — secrets,
// the disk, addon bindings — goes first, so the new version comes up with its
// configuration already in place.
func (r *Runtime) Deploy(ctx context.Context, spec rt.DeploySpec, out rt.Progress) error {
	defer r.InvalidateAppSummaries()
	app := spec.App
	space := spec.Space
	ns := r.ns(space)
	obj := r.name(space, app.Name)

	if err := r.ValidateApp(space, app.Name); err != nil {
		return rt.UserError{Err: err}
	}
	if err := r.ensureSpaceHome(ctx, space); err != nil {
		return err
	}
	if err := rt.CheckAppDomain(ctx, r, app, space, r.opts.Domain); err != nil {
		return err
	}
	out.Line("Setting up on the cluster…")
	// A file-browser helper holds the app's disk, and an RWO volume attaches
	// to one node at a time — leaving it running would strand the new pod on
	// another node with a multi-attach error until the rollout times out.
	if app.HasStorage() {
		if err := r.c.RemoveFilesHelper(ctx, ns, obj, space, app.Name); err != nil {
			return err
		}
	}
	if !app.HasStorage() && r.c.HasDataClaim(ctx, ns, obj, space, app.Name) {
		out.Line("· A disk from an earlier setup still exists (with its data). This deploy doesn't mount it — add its paths back under `storage:` in donkey.yaml to use it, or `donkey destroy` removes it.")
	}
	if app.HasStorage() {
		out.Line("· One disk (%s) for %s", app.Storage.Size, strings.Join(app.Storage.Paths, ", "))
		// An app that just gained a disk has to leave rolling updates behind.
		if err := r.c.SwitchToRecreate(ctx, ns, obj, space, app.Name); err != nil {
			return fmt.Errorf("couldn't switch %s to single-copy rollouts: %w", app.Name, err)
		}
	}
	// donkey.yaml is declarative about addons too: services belong to the
	// space, and an entry here means "this app uses that service" — created
	// on the spot when the space doesn't have it yet, before the app needs
	// the binding. An existing service is left alone: a second app naming it
	// simply joins it.
	for _, a := range app.Addons {
		if err := rt.CheckServiceAttachment(ctx, r, r.opts.CommonOfferings, space, app.Name, a.Name); err != nil {
			return err
		}
		connection, err := r.ServiceConnection(ctx, space, a.Name)
		if err != nil {
			return err
		}
		if err := (addons.Metadata{Type: connection.Type, Protocol: connection.Protocol}).CheckType(a.Type); err != nil {
			return rt.UserError{Err: err}
		}
		shared := connection.Binding
		if len(shared) == 0 {
			if a.External {
				return fmt.Errorf("addon %s runs elsewhere and its address isn't stored in donkey.yaml — connect it once with `donkey addon attach %s --url …`", a.Name, a.Type)
			}
			out.Line("Setting up %s (%s) for the space — the first time can take a minute…", a.Name, a.Type)
			if shared, _, err = r.EnsureService(ctx, space, rt.ServiceSpec{Type: a.Type, Name: a.Name, Parameters: a.ServiceParameters()}); err != nil {
				return err
			}
			out.Line("✓ %s is ready.", a.Name)
		}
		if err := addons.BindEnvironment(app, a.Name, shared); err != nil {
			return err
		}

		// The attachment copy is re-applied every deploy, so it stays current
		// with the space's service.
		if err := r.c.Apply(ctx, []k8sruntime.Object{attachmentSecret(space, app.Name, obj, ns, a.Name, shared)}); err != nil {
			return err
		}
	}
	in, err := r.manifestInput(spec, true)
	if err != nil {
		return err
	}
	if in.Domain != "" {
		if owner := r.hostOwner(ctx, space, in.Domain, app.Name); owner != "" {
			return fmt.Errorf("%s already answers on %s — two apps cannot share one address (give one of them a different `host`)", owner, in.Domain)
		}
	}
	r.resolveRouteTargets(ctx, space, &in)
	for i := range in.RouteTargets {
		if host := spec.Hosts[in.RouteTargets[i].App]; host != "" {
			in.RouteTargets[i].Host = host
		}
	}
	hosts := []string{in.Domain}
	for _, target := range in.RouteTargets {
		hosts = append(hosts, target.Host)
	}
	if err := rt.CheckSpaceDomains(ctx, r, space, hosts...); err != nil {
		return err
	}
	// Addons added in the console aren't in this donkey.yaml; the cluster
	// knows them by their binding secrets, and they keep working across
	// deploys instead of silently losing their env.
	if bindings, err := r.c.ListAddonBindings(ctx, ns, space, app.Name); err == nil {
		known := map[string]bool{}
		for _, n := range in.AddonSecretNames {
			known[n] = true
		}
		for _, b := range bindings {
			if n := manifests.AppBindingSecretName(obj, b.Name); !known[n] {
				in.AddonSecretNames = append(in.AddonSecretNames, n)
				out.Line("· Keeping %s, attached in the console", b.Name)
			}
		}
	}
	objs, err := manifests.AppObjects(in)
	if err != nil {
		return err
	}
	if err := r.c.preserveDataClaims(ctx, objs); err != nil {
		return err
	}
	// The Deployment is applied last. Everything it needs — secrets, the
	// disk, addon bindings — goes first, so the pods that start are never
	// briefly missing something they were promised.
	var deployment []k8sruntime.Object
	rest := objs[:0]
	for _, o := range objs {
		if o.GetObjectKind().GroupVersionKind().Kind == "Deployment" {
			deployment = append(deployment, o)
			continue
		}
		rest = append(rest, o)
	}
	if err := r.c.Apply(ctx, rest); err != nil {
		return fmt.Errorf("the cluster refused the changes: %v", err)
	}
	if err := r.c.Apply(ctx, deployment); err != nil {
		return fmt.Errorf("the cluster refused the changes: %v", err)
	}
	// A `routes:` entry that was dropped from donkey.yaml has to stop
	// serving; server-side apply only ever adds.
	if err := r.retractRemovedRoutes(ctx, ns, in); err != nil {
		return err
	}
	// An installation that stopped isolating spaces retracts the fence the
	// same way — apply never removes it.
	if err := r.syncSpaceIsolation(ctx, space); err != nil {
		return err
	}
	for _, route := range in.RouteTargets {
		out.Line("· Also answering https://%s%s", route.Host, route.Path)
	}
	// Server-side apply never deletes what the new set leaves out, so every
	// way an app can stop being exposed has to retract the objects itself.
	if in.Domain == "" {
		// Internal: no hostname, so the route goes — including when the app
		// used to have one. The gateway stops serving the hostname within
		// seconds; its cached certificate simply expires unused.
		if err := r.c.DeleteAppRoute(ctx, ns, space, app.Name); err != nil {
			return err
		}
		// The name siblings dial, which carries the space when the whole
		// installation shares a namespace.
		out.Line("· Internal only — the apps of this space reach it at http://%s", obj)
	} else if app.UsesOAuth() {
		out.Line("· API OAuth protection is in force.")
	} else if !app.Public {
		out.Line("· Login protection is in force.")
	}
	if err := r.reconcileAppTraffic(ctx, in); err != nil {
		return err
	}
	if spec.NoWait {
		return nil
	}

	out.Line("Waiting for the app to become healthy…")
	return r.c.WaitForRollout(ctx, ns, obj,
		manifests.Labels(space, app.Name, manifests.RoleWorkload))
}

// Render renders the app's objects without touching the cluster.
func (r *Runtime) Render(spec rt.DeploySpec) (string, error) {
	// Credentials only when the objects are going to the cluster. A dry run
	// prints its YAML to the user's terminal (and into CI logs), so the
	// registry password must not be in it.
	in, err := r.manifestInput(spec, false)
	if err != nil {
		return "", err
	}
	objs, err := manifests.AppObjects(in)
	if err != nil {
		return "", err
	}
	return manifests.RenderForInstallation(objs, r.opts.SystemNamespace)
}

func (r *Runtime) Destroy(ctx context.Context, space, app string, keepData bool) error {
	defer r.InvalidateAppSummaries()
	ns, obj := r.ns(space), r.name(space, app)
	// A file-browser helper still holding the disk would keep the volume in
	// Terminating — and it is a Pod, which the app's own delete never covers.
	if err := r.c.RemoveFilesHelper(ctx, ns, obj, space, app); err != nil {
		return err
	}
	if err := r.deleteAppTrafficObjects(ctx, space, app); err != nil {
		return err
	}
	return r.c.DeleteAppResources(ctx, ns, space, app, keepData)
}

// deleteAppTrafficObjects retracts the namespaced Certificate, generated TLS
// Secret and optional DNSEndpoint independently of workload objects. They
// live with the gateway even when the app lives in another namespace.
func (r *Runtime) deleteAppTrafficObjects(ctx context.Context, space, app string) error {
	if err := r.c.DeleteAppTraffic(ctx, r.opts.SystemNamespace, space, app); err != nil {
		return fmt.Errorf("removing the app's public traffic objects: %w", err)
	}
	return nil
}

// reconcileAppTraffic removes resources omitted by the active certificate or
// DNS choice after their replacements have applied.
func (r *Runtime) reconcileAppTraffic(ctx context.Context, in manifests.Input) error {
	if in.Domain == "" {
		return r.deleteAppTrafficObjects(ctx, in.Space, in.App.Name)
	}
	if err := r.c.DeleteObsoleteAppTraffic(ctx, r.opts.SystemNamespace, in.Space, in.App.Name,
		in.TLS.UsesCertManager(), in.ExternalDNS.Enabled()); err != nil {
		return fmt.Errorf("removing obsolete public traffic objects: %w", err)
	}
	return nil
}

// ReconcileAppTraffic brings every deployed app's Certificate and optional
// DNSEndpoint in line with installation settings. The route ConfigMap records
// the exact hostname, so an install can change certificate source or DNS
// publication without reconstructing donkey.yaml.
//
// The installer calls this after applying the control plane. Ordinary deploy,
// network and destroy operations reconcile the same objects for one app.
func (r *Runtime) ReconcileAppTraffic(ctx context.Context) error {
	apps, err := r.c.ListApps(ctx)
	if err != nil {
		return err
	}
	for _, app := range apps {
		net, err := r.c.AppNetwork(ctx, app.Namespace, r.name(app.Space, app.App), app.Space, app.App)
		if err != nil {
			return fmt.Errorf("reading the edge for %s/%s: %w", app.Space, app.App, err)
		}
		host := ""
		if net.RouteFound {
			host = net.Host
		}
		in := manifests.Input{
			App:             &config.AppConfig{Name: app.App},
			Space:           app.Space,
			Namespace:       app.Namespace,
			SystemNamespace: r.opts.SystemNamespace,
			ObjectName:      r.name(app.Space, app.App),
			Domain:          host,
			TLS:             r.opts.TLS,
			ExternalDNS:     r.opts.ExternalDNS,
		}
		objects := manifests.AppTrafficObjects(in)
		if err := r.c.Apply(ctx, objects); err != nil {
			return fmt.Errorf("applying public traffic for %s/%s: %w", app.Space, app.App, err)
		}
		if err := r.reconcileAppTraffic(ctx, in); err != nil {
			return fmt.Errorf("reconciling public traffic for %s/%s: %w", app.Space, app.App, err)
		}
	}
	return nil
}

// appHost is the address of an app — see config.AppHost for the rule.
func (r *Runtime) appHost(space string, app *config.AppConfig) string {
	return config.AppHost(app, space, r.opts.Domain)
}

func (r *Runtime) manifestInput(spec rt.DeploySpec, includeCreds bool) (manifests.Input, error) {
	app, space := spec.App, spec.Space
	in := manifests.Input{
		App:             app,
		Space:           space,
		Namespace:       r.ns(space),
		SharedNamespace: r.opts.Placement.Shared(),
		IsolateSpace:    r.opts.IsolateSpaces,
		RestrictEgress:  r.opts.RestrictEgress,
		SystemNamespace: r.opts.SystemNamespace,
		ObjectName:      r.name(space, app.Name),
		ImageRef:        spec.ImageRef,
		Decisions:       spec.Decisions,
		Stack:           spec.Stack,
		Security:        spec.Security,
		RegistryHost:    r.opts.Registry.Host(),
		Domain:          r.appHost(space, app),
		TLS:             r.opts.TLS,
		ExternalDNS:     r.opts.ExternalDNS,
	}

	if includeCreds {
		username, password, err := r.opts.Registry.Credentials()
		if err != nil {
			return in, err
		}
		in.RegistryUsername = username
		in.RegistryPassword = password
	}
	for _, a := range app.Addons {
		in.AddonSecretNames = append(in.AddonSecretNames, manifests.AppBindingSecretName(in.Name(), a.Name))
	}
	// Each route joins another app's address. The default shape is the
	// assumption; resolveRouteTargets corrects it against what that app
	// actually serves on, which it alone knows.
	for _, route := range app.Routes {
		host := spec.Hosts[route.App]
		if host == "" {
			host = route.App + "." + space + "." + r.opts.Domain
		}
		in.RouteTargets = append(in.RouteTargets, manifests.RouteTarget{
			App:         route.App,
			Host:        host,
			Path:        route.Path,
			StripPrefix: route.StripPrefix,
		})
	}
	return in, nil
}

// resolveRouteTargets replaces each target's assumed hostname with the one
// that app really answers on — it may sit at the space's own address, under
// a label of its own, or on a custom hostname. A target that isn't deployed
// yet keeps the assumption, which is what it will get when it is.
func (r *Runtime) resolveRouteTargets(ctx context.Context, space string, in *manifests.Input) {
	for i, target := range in.RouteTargets {
		net, err := r.c.AppNetwork(ctx, r.ns(space), r.name(space, target.App), space, target.App)
		if err == nil && net.RouteFound && net.Host != "" {
			in.RouteTargets[i].Host = net.Host
		}
	}
}

// hostOwner names the app already serving host in this space, or "" when it
// is free. Two apps on one hostname would both write a route for it and the
// gateway would serve whichever it sorted first — better to refuse than to
// let a deploy quietly take an address over.
func (r *Runtime) hostOwner(ctx context.Context, space, host, self string) string {
	cms, err := r.c.ListManagedRoutes(ctx)
	if err != nil {
		return ""
	}
	ns := r.ns(space)
	for _, cm := range cms {
		route, err := manifests.ReadRoute(&cm)
		if err != nil || cm.Namespace != ns || route.Host != host {
			continue
		}
		// Path routes share a hostname on purpose; only an app's own address
		// (path "/") owns it. Without this, a sibling's `routes:` entry on
		// this host would block the host's own app from deploying.
		if route.Path != "/" {
			continue
		}
		if owner := cm.Labels[manifests.LabelApp]; owner != "" && owner != self {
			return owner
		}
	}
	return ""
}

// retractRemovedRoutes deletes the shared-address routes an app used to
// claim and no longer does. Server-side apply never removes what a new set
// leaves out, so dropping a `routes:` entry would otherwise keep serving it
// forever.
func (r *Runtime) retractRemovedRoutes(ctx context.Context, ns string, in manifests.Input) error {
	keep := map[string]bool{}
	for _, route := range in.RouteTargets {
		keep[manifests.PathRouteName(in.Name(), route.App, route.Path)] = true
	}
	names, err := r.c.ListRouteNames(ctx, ns, in.Space, in.App.Name)
	if err != nil {
		return err
	}
	for _, name := range names {
		if keep[name] {
			continue
		}
		if err := r.c.DeleteObject(ctx, "ConfigMap", ns, name,
			manifests.Labels(in.Space, in.App.Name, manifests.RolePathRoute)); err != nil {
			return err
		}
	}
	return nil
}

// ApplyNetwork changes how the app is reached — without a deploy. The route
// and optional Certificate/DNSEndpoint are re-rendered from cluster state;
// existing shared-address routes keep serving under their existing names.
func (r *Runtime) ApplyNetwork(ctx context.Context, cfg *config.AppConfig) (string, error) {
	defer r.InvalidateAppSummaries()
	space, app := cfg.Space, cfg.Name
	if err := rt.CheckAppDomain(ctx, r, cfg, space, r.opts.Domain); err != nil {
		return "", err
	}
	ns, obj := r.ns(space), r.name(space, app)
	st, err := r.c.AppStatus(ctx, ns, obj, space, app)
	if err != nil {
		return "", err
	}
	net, err := r.c.AppNetwork(ctx, ns, obj, space, app)
	if err != nil {
		return "", err
	}
	in, err := r.manifestInput(rt.DeploySpec{App: cfg, Space: space, ImageRef: st.Image}, false)
	if err != nil {
		return "", err
	}
	// The same guard a deploy applies: without it the console could hand this
	// app an address another one already serves, and the gateway would answer
	// with whichever it sorted first.
	if in.Domain != "" {
		if owner := r.hostOwner(ctx, space, in.Domain, app); owner != "" {
			return "", rt.UserError{Err: fmt.Errorf("%s already answers on %s — two apps cannot share one address", owner, in.Domain)}
		}
	}

	// Re-render: the app's own route and public traffic objects unless
	// internal, and every existing path route under its existing name.
	hosts := []string{in.Domain}
	for _, route := range net.Routes {
		hosts = append(hosts, route.Host)
	}
	if err := rt.CheckSpaceDomains(ctx, r, space, hosts...); err != nil {
		return "", err
	}
	var objs []k8sruntime.Object
	if in.Domain != "" {
		objs = append(objs, manifests.AppRoute(in))
		objs = append(objs, manifests.AppTrafficObjects(in)...)
	}
	for _, pr := range net.Routes {
		objs = append(objs, manifests.PathRouteNamed(in, pr.Name, manifests.RouteTarget{Host: pr.Host, Path: pr.Path, StripPrefix: pr.StripPrefix}))
	}
	if err := r.c.Apply(ctx, objs); err != nil {
		return "", err
	}

	if in.Domain == "" {
		if err := r.c.DeleteAppRoute(ctx, ns, space, app); err != nil {
			return "", err
		}
	}
	if err := r.c.SetAppNetworkMetadata(ctx, ns, obj, space, app, cfg.GatewayMode(), cfg.CORS); err != nil {
		return "", err
	}
	if err := r.reconcileAppTraffic(ctx, in); err != nil {
		return "", err
	}
	return "", nil
}

func (r *Runtime) Network(ctx context.Context, space, app string) (*rt.NetworkState, error) {
	return r.c.AppNetwork(ctx, r.ns(space), r.name(space, app), space, app)
}

// ReconcileAppRoutes changes only access fields, preserving addresses and backends.
func (r *Runtime) ReconcileAppRoutes(ctx context.Context, space, app string, protect bool) error {
	defer r.InvalidateAppSummaries()
	ns, obj := r.ns(space), r.name(space, app)
	net, err := r.c.AppNetwork(ctx, ns, obj, space, app)
	if err != nil {
		return err
	}
	if net.RouteFound {
		cm, err := r.c.appRouteConfigMap(ctx, ns, obj, space, app)
		if err != nil {
			return err
		}
		if err := r.c.reconcileRouteAccess(ctx, cm, protect); err != nil {
			return err
		}
	}
	for _, pr := range net.Routes {
		cm, err := r.c.Clientset.CoreV1().ConfigMaps(ns).Get(ctx, pr.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := r.c.requireOwnedObject("ConfigMap", ns, pr.Name, cm.Labels, manifests.Labels(space, app, manifests.RolePathRoute)); err != nil {
			return err
		}
		if err := r.c.reconcileRouteAccess(ctx, cm, protect); err != nil {
			return err
		}
	}
	return nil
}
