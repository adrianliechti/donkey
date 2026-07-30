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
	// This writes an object named from the pair, so the pair has to pass the
	// same rules a deploy enforces — otherwise a crafted space like "a--b"
	// plants a secret whose name another space's app could ambiguously own.
	if err := r.ValidateApp(space, app); err != nil {
		return rt.UserError{Err: err}
	}
	return r.c.SetEnv(ctx, r.ns(space), space, r.name(space, app), env)
}

// --- App metadata ---------------------------------------------------------

func (r *Runtime) AppMeta(ctx context.Context, space, app, key string) (string, error) {
	return r.c.DeploymentAnnotation(ctx, r.ns(space), r.name(space, app), key)
}

func (r *Runtime) SetAppMeta(ctx context.Context, space, app, key string, value *string) error {
	return r.c.SetAnnotations(ctx, "Deployment", r.ns(space), r.name(space, app), map[string]*string{key: value})
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
