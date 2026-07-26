package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// preDeployTimeout bounds a pre-deploy step. Migrations on a large table are
// slow but not endless, and a step that hangs would otherwise hold the deploy
// open indefinitely.
const preDeployTimeout = 15 * time.Minute

// Deploy applies the app's objects with the given image and waits for the
// rollout (unless spec.NoWait). Everything the Deployment needs — secrets,
// the disk, addon bindings — goes first, so a pre-deploy step runs with the
// new configuration while the old version is still the one serving.
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
	// on the spot when the space doesn't have it yet, before the app (and
	// its pre-deploy step) needs the binding. An existing service is left
	// alone: a second app naming it simply joins it.
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
	// disk, addon bindings — goes first, so a pre-deploy step runs with the
	// new configuration while the old version is still the one serving.
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
	if len(app.PreDeploy) > 0 {
		out.Line("Running the pre-deploy step…")
		if err := r.c.RunJob(ctx, manifests.PreDeployJob(in), preDeployTimeout); err != nil {
			// The Deployment was deliberately not touched, so whatever is
			// live stays live — that is the point of running this first.
			return fmt.Errorf("the pre-deploy step failed, so nothing was changed and the running version keeps serving: %w", err)
		}
		out.Line("✓ Pre-deploy step finished.")
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
	} else if !app.Public && r.opts.AuthAvailable {
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

// appHost is the address of an app: <app>.<space>.<domain>. The space in the
// middle is what lets two teams both have a "web". An internal app has no
// address at all — its siblings reach it by service name.
func (r *Runtime) appHost(space string, app *config.AppConfig) string {
	if app.Internal {
		return ""
	}
	if app.Domain != "" {
		return app.Domain
	}
	return app.Name + "." + space + "." + r.opts.Domain
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
	// The donkey gateway is every route's real backend; whether it can also
	// offer a login depends on the installation having an IdP.
	in.Gateway = &manifests.GatewayRef{
		Service:   manifests.GatewayAppName,
		Namespace: manifests.SystemNamespace,
		Auth:      r.opts.AuthAvailable,
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
	// Each route joins another app's address, so its hostname is that app's.
	// Only the default hostname is resolvable from here — a target with its
	// own `domain:` would need looking up.
	for _, route := range app.Routes {
		in.RouteTargets = append(in.RouteTargets, manifests.RouteTarget{
			App:  route.App,
			Host: route.App + "." + space + "." + r.opts.Domain,
			Path: route.Path,
		})
	}
	return in, nil
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
