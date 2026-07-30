package kubernetes

import (
	"context"
	"fmt"
	"io"
	"net/http"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// Options is what the Kubernetes runtime needs to know about the
// installation — how spaces map to namespaces, where apps are served, and
// which shared pieces (gateway, issuer) their objects reference.
type Options struct {
	// Placement decides where a space's objects live and what they are
	// called: a namespace per space, or one namespace for the installation.
	Placement manifests.Placement
	// SystemNamespace is where the control plane lives. It is also this
	// installation's identity — see manifests.LabelInstance.
	SystemNamespace string
	// Domain serves apps as <app>.<space>.<domain>.
	Domain string
	// Registry feeds the per-app image pull secret.
	Registry config.Registry
}

// Runtime runs donkey's apps on a Kubernetes cluster. It deliberately does
// not embed Client: every Provider verb takes a space where the Client takes
// a namespace, and an inherited method with the wrong meaning would satisfy
// the interface silently.
type Runtime struct {
	c    *Client
	opts Options
}

var _ rt.Provider = (*Runtime)(nil)

func NewRuntime(client *Client, opts Options) *Runtime {
	if opts.SystemNamespace == "" {
		opts.SystemNamespace = manifests.DefaultSystemNamespace
	}
	// The client speaks for this installation, and — when the installation was
	// given its namespaces rather than making them — sees only those.
	client = client.ForInstallation(opts.SystemNamespace, opts.Placement.SharedNamespace)
	return &Runtime{c: client, opts: opts}
}

// Client exposes the underlying cluster client for Kubernetes-specific
// callers (the installer, the gateway) that need more than the Provider.
func (r *Runtime) Client() *Client { return r.c }

// ns is where a space lives: all its apps share the namespace.
func (r *Runtime) ns(space string) string { return r.opts.Placement.Namespace(space) }

// name is what one of the space's things is called on the cluster. It is the
// plain name where the space has a namespace to itself, and carries the space
// where the whole installation shares one — a namespace holds one Service
// called "api" and no more.
//
// Object names and label values are qualified together, on purpose: every
// selector donkey already writes (and every destructive DeleteCollection
// among them) then addresses one space's objects without a single call site
// having to remember to narrow it.
func (r *Runtime) name(space, name string) string { return r.opts.Placement.Name(space, name) }

// unname is the inverse — what to show a person, and what they typed.
func (r *Runtime) unname(space, objectName string) string {
	return r.opts.Placement.Unname(space, objectName)
}

// sharedOwner is the pseudo-app a space's shared services belong to.
func (r *Runtime) sharedOwner(space string) string {
	return r.name(space, addons.SharedOwner)
}

// --- Spaces ---------------------------------------------------------------

func (r *Runtime) ValidateSpace(space string) error {
	if !config.ValidName(space) {
		return fmt.Errorf("%q is not a valid space name — lowercase letters, digits and dashes (max 40 chars)", space)
	}
	// With the default prefix, the space "system" resolves to donkey-system —
	// a crafted (or innocently repo-named) space must never reach the control
	// plane's objects. Where every space shares one namespace there is no such
	// mapping, and nothing to collide with.
	if !r.opts.Placement.Shared() && r.ns(space) == r.opts.SystemNamespace {
		return fmt.Errorf("the space name %q is reserved for donkey itself — pick another (space: <name> in donkey.yaml)", space)
	}
	// A shared namespace joins the space onto every name in it, so the space
	// has to be joinable at all — and short enough to leave room for an app.
	return r.opts.Placement.ValidateNames(space, "")
}

// ValidateApp checks a space and app name together. Apart they are both
// valid; joined into one object name they may be too long, which is something
// only the pair can answer.
func (r *Runtime) ValidateApp(space, app string) error {
	if err := r.ValidateSpace(space); err != nil {
		return err
	}
	if !config.ValidName(app) {
		return fmt.Errorf("%q is not a valid app name — lowercase letters, digits and dashes (max 40 chars)", app)
	}
	return r.opts.Placement.ValidateNames(space, app)
}

func (r *Runtime) CheckSpaceFree(ctx context.Context, space string) error {
	if err := r.ValidateSpace(space); err != nil {
		return err
	}
	// Nothing to check when the namespace was given to donkey rather than
	// made by it: a space is objects inside it, and taking a name that is
	// free is what every other space did too.
	if r.opts.Placement.Shared() {
		return nil
	}
	return r.c.CheckNamespaceFree(ctx, r.ns(space))
}

func (r *Runtime) ListSpaces(ctx context.Context) ([]string, error) {
	refs, err := r.spaceRefs(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ref := range refs {
		space := ref.Space
		if space == "" {
			space = r.opts.Placement.SpaceOf(ref.Namespace)
		}
		if space != "" {
			out = append(out, space)
		}
	}
	return out, nil
}

// spaceRefs finds the spaces of this installation, wherever they are
// recorded: a namespace each, or a marker object in the one shared namespace.
func (r *Runtime) spaceRefs(ctx context.Context) ([]AppRef, error) {
	if r.opts.Placement.Shared() {
		return r.c.ListSpaceMarkers(ctx, r.opts.Placement.SharedNamespace)
	}
	return r.c.ListSpaces(ctx)
}

func (r *Runtime) CreateSpace(ctx context.Context, space string) error {
	if err := r.ValidateSpace(space); err != nil {
		return err
	}
	return r.c.Apply(ctx, []k8sruntime.Object{r.opts.Placement.SpaceObject(space)})
}

func (r *Runtime) DeleteSpace(ctx context.Context, space string) error {
	if err := r.ValidateSpace(space); err != nil {
		return err
	}
	if r.opts.Placement.Shared() {
		return r.c.DeleteSpaceObjects(ctx, r.ns(space), space)
	}
	return r.c.DeleteSpace(ctx, r.ns(space))
}

// --- Apps -----------------------------------------------------------------

func (r *Runtime) ListApps(ctx context.Context) ([]rt.AppRef, error) {
	refs, err := r.c.ListApps(ctx)
	if err != nil {
		return nil, err
	}
	var out []rt.AppRef
	for _, ref := range refs {
		space := ref.Space
		if space == "" {
			space = r.opts.Placement.SpaceOf(ref.Namespace)
		}
		out = append(out, rt.AppRef{Space: space, App: r.unname(space, ref.App)})
	}
	return out, nil
}

func (r *Runtime) Status(ctx context.Context, space, app string) (*rt.AppStatus, error) {
	return r.c.AppStatus(ctx, r.ns(space), r.name(space, app))
}

func (r *Runtime) HasData(ctx context.Context, space, app string) bool {
	return r.c.HasDataClaim(ctx, r.ns(space), r.name(space, app))
}

func (r *Runtime) Restart(ctx context.Context, space, app string) (bool, error) {
	return r.c.RestartApp(ctx, r.ns(space), r.name(space, app))
}

func (r *Runtime) Logs(ctx context.Context, space, app string, opts rt.LogOptions, out io.Writer) error {
	return r.c.StreamLogs(ctx, r.ns(space), r.name(space, app), opts, out)
}

func (r *Runtime) Evidence(ctx context.Context, space, app string) rt.Evidence {
	return r.c.RolloutEvidenceFor(ctx, r.ns(space), r.name(space, app))
}

// --- Env ------------------------------------------------------------------

func (r *Runtime) GetEnv(ctx context.Context, space, app string) (map[string]string, error) {
	return r.c.GetEnv(ctx, r.ns(space), r.name(space, app))
}

func (r *Runtime) SetEnv(ctx context.Context, space, app string, env map[string]string) error {
	return r.c.SetEnv(ctx, r.ns(space), space, r.name(space, app), env)
}

// --- App metadata ---------------------------------------------------------

func (r *Runtime) AppMeta(ctx context.Context, space, app, key string) (string, error) {
	return r.c.DeploymentAnnotation(ctx, r.ns(space), r.name(space, app), key)
}

func (r *Runtime) SetAppMeta(ctx context.Context, space, app, key string, value *string) error {
	return r.c.SetAnnotations(ctx, "Deployment", r.ns(space), r.name(space, app), map[string]*string{key: value})
}

// --- Services (addons) ----------------------------------------------------

func (r *Runtime) ServiceBinding(ctx context.Context, space, owner, name string) (map[string]string, error) {
	return r.c.GetSecretData(ctx, r.ns(space), addons.SecretName(r.name(space, owner), name))
}

// EnsureService creates (or re-applies) one service of the space —
// provisioned in-cluster, or external as a binding-only Secret — and returns
// its binding values.
func (r *Runtime) EnsureService(ctx context.Context, space string, spec rt.ServiceSpec) (map[string]string, string, string, error) {
	ns := r.ns(space)
	addon, err := addons.Get(spec.Type)
	if err != nil {
		return nil, "", "", rt.UserError{Err: err}
	}
	if !config.ValidName(spec.Name) {
		return nil, "", "", rt.UserError{Err: fmt.Errorf("%q is not a valid service name — lowercase letters, digits and dashes", spec.Name)}
	}
	if err := r.opts.Placement.ValidateNames(space, spec.Name); err != nil {
		return nil, "", "", rt.UserError{Err: err}
	}
	var binding map[string]string
	var version, storage string
	if spec.External {
		binding, err = addon.ExternalBinding(spec.URL)
		if err != nil {
			return nil, "", "", rt.UserError{Err: err}
		}
		objs := []k8sruntime.Object{
			r.opts.Placement.SpaceObject(space),
			bindingSecret(space, r.sharedOwner(space), ns, spec.Name, binding),
		}
		if err := r.c.Apply(ctx, objs); err != nil {
			return nil, "", "", err
		}
	} else {
		// The service takes its plain name, reachable as e.g. "postgres" the
		// way siblings reach each other — so it must not seize a name an app
		// (or a differently-named service) already holds.
		if component, exists, err := r.c.DeploymentComponent(ctx, ns, r.name(space, spec.Name)); err != nil {
			return nil, "", "", err
		} else if exists && component != addons.Component(spec.Name) {
			return nil, "", "", rt.UserError{Err: fmt.Errorf("the name %q is taken by an app in this space — pick another for the service", spec.Name)}
		}
		in, err := r.provisionService(ctx, space, ns, spec)
		if err != nil {
			return nil, "", "", err
		}
		binding, version, storage = addon.Binding(in), in.Version, in.Storage
	}
	// Re-creating an external service may change its address — the copies the
	// attached apps hold have to follow, and their pods restart to see the
	// new values. (A managed re-apply keeps host and credential, so the
	// copies come out identical and nothing restarts.)
	attached, err := r.c.ListAddonAttachments(ctx, ns, space, addons.Component(spec.Name))
	if err != nil {
		return nil, "", "", err
	}
	for _, app := range attached {
		if err := r.c.Apply(ctx, []k8sruntime.Object{attachmentSecret(space, app, ns, spec.Name, binding)}); err != nil {
			return nil, "", "", err
		}
		if spec.External {
			if _, err := r.c.RestartApp(ctx, ns, app); err != nil {
				return nil, "", "", err
			}
		}
	}
	return binding, version, storage, nil
}

// provisionService creates (or re-applies) one managed addon instance —
// owned by the space's shared pseudo-app — with its binding secret, and waits
// until it serves.
func (r *Runtime) provisionService(ctx context.Context, space, ns string, spec rt.ServiceSpec) (addons.Input, error) {
	addon, err := addons.Get(spec.Type)
	if err != nil {
		return addons.Input{}, err
	}
	in := addons.Input{
		App: r.sharedOwner(space), Space: space, Namespace: ns,
		Name: spec.Name, ObjectName: r.name(space, spec.Name),
		Version: spec.Version, Storage: spec.Storage,
		AIProxyURL: r.aiProxyURL(),
	}
	if in.Version == "" {
		in.Version = addon.DefaultVersion()
	}
	if in.Storage == "" {
		in.Storage = addon.DefaultStorage()
	}
	existing, err := r.c.GetSecretData(ctx, ns, addons.SecretName(in.App, spec.Name))
	if err != nil {
		return in, err
	}
	// Reuse the credential the instance was initialized with — rotating it
	// here would lock the service out of its own kept data.
	if pw, ok := existing[addon.CredentialKey()]; ok {
		in.Password = pw
	} else if in.Password, err = addons.NewPassword(); err != nil {
		return in, err
	}
	objs := []k8sruntime.Object{
		r.opts.Placement.SpaceObject(space),
		bindingSecret(space, in.App, ns, in.Name, addon.Binding(in)),
	}
	workload, err := addon.Workload(in)
	if err != nil {
		return in, err
	}
	if workload != nil {
		addonObjs, err := addonObjects(in, workload)
		if err != nil {
			return in, err
		}
		objs = append(objs, addonObjs...)
	}
	if err := r.c.Apply(ctx, objs); err != nil {
		return in, err
	}
	// A binding-only service (like openai) runs nothing — there is no
	// rollout to wait for.
	if workload == nil {
		return in, nil
	}
	return in, r.c.WaitForRollout(ctx, ns, in.HostName())
}

func (r *Runtime) AttachService(ctx context.Context, space, app, name string, binding map[string]string) (bool, error) {
	ns := r.ns(space)
	obj := r.name(space, app)
	if err := r.c.Apply(ctx, []k8sruntime.Object{attachmentSecret(space, obj, ns, name, binding)}); err != nil {
		return false, err
	}
	// An app that is already deployed picks the binding up on the spot; one
	// that isn't gets it wired by its first deploy.
	return r.c.SetAddonEnvFrom(ctx, ns, obj, addons.SecretName(obj, name), true)
}

func (r *Runtime) DetachService(ctx context.Context, space, app, name string) error {
	ns := r.ns(space)
	obj := r.name(space, app)
	// Unwire before deleting: a pod template that still references the
	// binding secret would fail to start once the secret is gone.
	if _, err := r.c.SetAddonEnvFrom(ctx, ns, obj, addons.SecretName(obj, name), false); err != nil {
		return err
	}
	return r.c.DetachAddon(ctx, ns, obj, addons.Component(name))
}

func (r *Runtime) RemoveService(ctx context.Context, space, name string, keepData bool) ([]string, error) {
	ns := r.ns(space)
	attached, err := r.c.ListAddonAttachments(ctx, ns, space, addons.Component(name))
	if err != nil {
		return nil, err
	}
	var apps []string
	for _, obj := range attached {
		if _, err := r.c.SetAddonEnvFrom(ctx, ns, obj, addons.SecretName(obj, name), false); err != nil {
			return nil, err
		}
		if err := r.c.DetachAddon(ctx, ns, obj, addons.Component(name)); err != nil {
			return nil, err
		}
		apps = append(apps, r.unname(space, obj))
	}
	if err := r.c.DeleteComponent(ctx, ns, r.sharedOwner(space), addons.Component(name), keepData); err != nil {
		return nil, err
	}
	return apps, nil
}

func (r *Runtime) ServiceAttachments(ctx context.Context, space, name string) ([]string, error) {
	attached, err := r.c.ListAddonAttachments(ctx, r.ns(space), space, addons.Component(name))
	if err != nil {
		return nil, err
	}
	apps := make([]string, 0, len(attached))
	for _, obj := range attached {
		apps = append(apps, r.unname(space, obj))
	}
	return apps, nil
}

func (r *Runtime) ServiceEndpoint(ctx context.Context, space, name string) (string, int32, error) {
	return r.c.ServiceEndpoint(ctx, r.ns(space), r.name(space, name))
}

func (r *Runtime) ListServices(ctx context.Context, space, owner string) ([]rt.ServiceBindingInfo, error) {
	return r.c.ListAddonBindings(ctx, r.ns(space), r.name(space, owner))
}

func (r *Runtime) SharedServiceValues(ctx context.Context, key string) ([]string, error) {
	return r.c.SharedBindingValues(ctx, addons.SharedOwner, key)
}

// aiProxyURL is where apps reach donkey-server's AI proxy on this
// installation — its own control plane, not whichever one answers first.
func (r *Runtime) aiProxyURL() string {
	return fmt.Sprintf("http://%s.%s.svc/ai/v1", manifests.ServerName, r.opts.SystemNamespace)
}

// --- Files ----------------------------------------------------------------

func (r *Runtime) ListFiles(ctx context.Context, space, app, path string) ([]rt.FileEntry, error) {
	return r.c.ListFiles(ctx, r.ns(space), r.name(space, app), path)
}

func (r *Runtime) StorageFolders(ctx context.Context, space, app string) ([]rt.StorageFolder, error) {
	return r.c.StorageFolders(ctx, r.ns(space), r.name(space, app))
}

func (r *Runtime) ReadFile(ctx context.Context, space, app, path string, out io.Writer) error {
	return r.c.ReadFile(ctx, r.ns(space), r.name(space, app), path, out)
}

func (r *Runtime) WriteFile(ctx context.Context, space, app, path string, in io.Reader) error {
	return r.c.WriteFile(ctx, r.ns(space), r.name(space, app), path, in)
}

func (r *Runtime) RemoveFile(ctx context.Context, space, app, path string) error {
	return r.c.RemoveFile(ctx, r.ns(space), r.name(space, app), path)
}

func (r *Runtime) MoveFile(ctx context.Context, space, app, from, to string) error {
	return r.c.MoveFile(ctx, r.ns(space), r.name(space, app), from, to)
}

func (r *Runtime) MakeDir(ctx context.Context, space, app, path string) error {
	return r.c.MakeDir(ctx, r.ns(space), r.name(space, app), path)
}

// --- Health ---------------------------------------------------------------

func (r *Runtime) Checks(ctx context.Context) []rt.Check {
	var checks []rt.Check

	// Every app rides through the donkey gateway — it is the edge, so if it
	// is down, so is every app, which is exactly what doctor exists to say
	// plainly.
	dg := rt.Check{Name: "App gateway (donkey-gateway)"}
	gwURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/healthz",
		manifests.GatewayHealthServiceName, r.opts.SystemNamespace, manifests.GatewayHealthServicePort)
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, gwURL, nil); err != nil {
		dg.Detail = err.Error()
	} else if res, err := http.DefaultClient.Do(req); err != nil {
		dg.Detail = err.Error()
	} else {
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			dg.OK = true
			dg.Detail = "routing"
		} else {
			dg.Detail = fmt.Sprintf("answered %d", res.StatusCode)
		}
	}
	checks = append(checks, dg)

	// Disks are the quiet dependency: without a default StorageClass, apps
	// with storage and provisioned addons never get one.
	sc := rt.Check{Name: "Storage (default StorageClass)"}
	switch ok, err := r.c.HasDefaultStorageClass(ctx); {
	// StorageClasses are cluster-scoped, and an installation confined to the
	// namespaces it was given holds nothing cluster-scoped on purpose. Not
	// being able to look is the configuration working, not a fault — so this
	// says what it can't see rather than reporting a failure nobody can fix.
	case err != nil && r.c.Confined():
		sc.OK = true
		sc.Detail = "not checked — this installation has no cluster-scoped permissions"
	case err != nil:
		sc.Detail = err.Error()
	case !ok:
		sc.Detail = "no default StorageClass — apps with storage and provisioned addons won't get disks"
	default:
		sc.OK = true
		sc.Detail = "available"
	}
	checks = append(checks, sc)

	return checks
}
