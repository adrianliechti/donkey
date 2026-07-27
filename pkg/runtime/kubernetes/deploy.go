package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	app := spec.App
	space := spec.Space
	ns := r.ns(space)

	out.Line("Setting up on the cluster…")
	// A file-browser helper holds the app's disk, and an RWO volume attaches
	// to one node at a time — leaving it running would strand the new pod on
	// another node with a multi-attach error until the rollout times out.
	if app.HasStorage() {
		if err := r.c.RemoveFilesHelper(ctx, ns, app.Name); err != nil {
			return err
		}
	}
	if !app.HasStorage() && r.c.HasDataClaim(ctx, ns, app.Name) {
		out.Line("· A disk from an earlier setup still exists (with its data). This deploy doesn't mount it — add its paths back under `storage:` in donkey.yaml to use it, or `donkey destroy` removes it.")
	}
	if app.HasStorage() {
		out.Line("· One disk (%s) for %s", app.Storage.Size, strings.Join(app.Storage.Paths, ", "))
		// An app that just gained a disk has to leave rolling updates behind.
		if err := r.c.SwitchToRecreate(ctx, ns, app.Name); err != nil {
			return fmt.Errorf("couldn't switch %s to single-copy rollouts: %w", app.Name, err)
		}
	}
	// donkey.yaml is declarative about addons too: services belong to the
	// space, and an entry here means "this app uses that service" — created
	// on the spot when the space doesn't have it yet, before the app needs
	// the binding. An existing service is left alone: a second app naming it
	// simply joins it.
	for _, a := range app.Addons {
		shared, err := r.c.GetSecretData(ctx, ns, addons.SecretName(addons.SharedOwner, a.Name))
		if err != nil {
			return err
		}
		if len(shared) == 0 {
			if a.External {
				return fmt.Errorf("addon %s runs elsewhere and its address isn't stored in donkey.yaml — connect it once with `donkey addon attach %s --url …`", a.Name, a.Type)
			}
			out.Line("Setting up %s (%s) for the space — the first time can take a minute…", a.Name, a.Type)
			if shared, _, _, err = r.EnsureService(ctx, space, rt.ServiceSpec{Type: a.Type, Name: a.Name, Version: a.Version, Storage: a.Storage}); err != nil {
				return err
			}
			out.Line("✓ %s is ready.", a.Name)
		}
		// The attachment copy is re-applied every deploy, so it stays current
		// with the space's service.
		if err := r.c.Apply(ctx, []k8sruntime.Object{attachmentSecret(space, app.Name, ns, a.Name, shared)}); err != nil {
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
	// Addons added in the console aren't in this donkey.yaml; the cluster
	// knows them by their binding secrets, and they keep working across
	// deploys instead of silently losing their env.
	if bindings, err := r.c.ListAddonBindings(ctx, ns, app.Name); err == nil {
		known := map[string]bool{}
		for _, n := range in.AddonSecretNames {
			known[n] = true
		}
		for _, b := range bindings {
			if n := addons.SecretName(app.Name, b.Name); !known[n] {
				in.AddonSecretNames = append(in.AddonSecretNames, n)
				out.Line("· Keeping %s, attached in the console", b.Name)
			}
		}
	}
	objs, err := manifests.AppObjects(in)
	if err != nil {
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
	for _, route := range in.RouteTargets {
		out.Line("· Also answering https://%s%s", route.Host, route.Path)
	}
	// Server-side apply never deletes what the new set leaves out, so every
	// way an app can stop being exposed has to retract the objects itself.
	if in.Domain == "" {
		// Internal: no hostname, so the route goes — including when the app
		// used to have one. The gateway stops serving the hostname within
		// seconds; its cached certificate simply expires unused.
		if err := r.c.DeleteObject(ctx, "ConfigMap", ns, app.Name); err != nil {
			return err
		}
		out.Line("· Internal only — the apps of this space reach it at http://%s", app.Name)
	} else if !app.Public {
		out.Line("· Login protection is in force.")
	}
	if spec.NoWait {
		return nil
	}

	out.Line("Waiting for the app to become healthy…")
	return r.c.WaitForRollout(ctx, ns, app.Name)
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
	return manifests.Render(objs)
}

func (r *Runtime) Destroy(ctx context.Context, space, app string, keepData bool) error {
	ns := r.ns(space)
	// A file-browser helper still holding the disk would keep the volume in
	// Terminating — and it is a Pod, which the app's own delete never covers.
	if err := r.c.RemoveFilesHelper(ctx, ns, app); err != nil {
		return err
	}
	return r.c.DeleteAppResources(ctx, ns, app, keepData)
}

// appHost is the address of an app — see config.AppHost for the rule.
func (r *Runtime) appHost(space string, app *config.AppConfig) string {
	return config.AppHost(app, space, r.opts.Domain)
}

func (r *Runtime) manifestInput(spec rt.DeploySpec, includeCreds bool) (manifests.Input, error) {
	app, space := spec.App, spec.Space
	in := manifests.Input{
		App:          app,
		Space:        space,
		Namespace:    r.ns(space),
		ImageRef:     spec.ImageRef,
		Decisions:    spec.Decisions,
		RegistryHost: r.opts.Registry.Host(),
		Domain:       r.appHost(space, app),
	}
	if includeCreds {
		password, err := r.opts.Registry.Password()
		if err != nil {
			return in, err
		}
		in.RegistryUsername = r.opts.Registry.Username
		in.RegistryPassword = password
	}
	for _, a := range app.Addons {
		in.AddonSecretNames = append(in.AddonSecretNames, addons.SecretName(app.Name, a.Name))
	}
	// Each route joins another app's address. The default shape is the
	// assumption; resolveRouteTargets corrects it against what that app
	// actually serves on, which it alone knows.
	for _, route := range app.Routes {
		in.RouteTargets = append(in.RouteTargets, manifests.RouteTarget{
			App:  route.App,
			Host: route.App + "." + space + "." + r.opts.Domain,
			Path: route.Path,
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
		net, err := r.c.AppNetwork(ctx, r.ns(space), target.App)
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
		if cm.Namespace != ns || cm.Annotations[manifests.AnnotationHost] != host {
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
		keep[manifests.PathRouteName(in.App.Name, route.App, route.Path)] = true
	}
	names, err := r.c.ListRouteNames(ctx, ns, in.App.Name)
	if err != nil {
		return err
	}
	for _, name := range names {
		if keep[name] {
			continue
		}
		if err := r.c.DeleteObject(ctx, "ConfigMap", ns, name); err != nil {
			return err
		}
	}
	return nil
}

// ApplyNetwork changes how the app is reached — without a deploy. The route,
// certificate and gateway grant are re-rendered from cluster state; existing
// shared-address routes keep serving under their existing names (their
// exposure follows the app's).
func (r *Runtime) ApplyNetwork(ctx context.Context, cfg *config.AppConfig) (string, error) {
	space, app := cfg.Space, cfg.Name
	ns := r.ns(space)
	st, err := r.c.AppStatus(ctx, ns, app)
	if err != nil {
		return "", err
	}
	net, err := r.c.AppNetwork(ctx, ns, app)
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

	// Re-render: the app's own route unless internal, and every existing
	// path route under its existing name.
	var objs []k8sruntime.Object
	if in.Domain != "" {
		objs = append(objs, manifests.AppRoute(in))
	}
	for _, pr := range net.Routes {
		objs = append(objs, manifests.PathRouteNamed(in, pr.Name, manifests.RouteTarget{Host: pr.Host, Path: pr.Path}))
	}
	if err := r.c.Apply(ctx, objs); err != nil {
		return "", err
	}

	if in.Domain == "" {
		if err := r.c.DeleteObject(ctx, "ConfigMap", ns, app); err != nil {
			return "", err
		}
	}
	return "", nil
}

func (r *Runtime) Network(ctx context.Context, space, app string) (*rt.NetworkState, error) {
	return r.c.AppNetwork(ctx, r.ns(space), app)
}

// SetAccess narrows (or reopens) who may open a login-protected app. It is
// an annotation change on the app's routes — the gateway picks it up within
// seconds, no deploy, no restart.
func (r *Runtime) SetAccess(ctx context.Context, space, app string, access *config.AccessConfig) error {
	ns := r.ns(space)
	net, err := r.c.AppNetwork(ctx, ns, app)
	if err != nil {
		return err
	}
	var value *string
	if access != nil {
		b, err := json.Marshal(access)
		if err != nil {
			return err
		}
		v := string(b)
		value = &v
	}
	patch := map[string]*string{manifests.AnnotationAccess: value}
	if net.Protected {
		if err := r.c.SetAnnotations(ctx, "ConfigMap", ns, app, patch); err != nil {
			return err
		}
	}
	for _, pr := range net.Routes {
		if !pr.Protected {
			continue
		}
		if err := r.c.SetAnnotations(ctx, "ConfigMap", ns, pr.Name, patch); err != nil {
			return err
		}
	}
	return nil
}
