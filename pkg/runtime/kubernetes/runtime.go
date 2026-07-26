package kubernetes

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

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
	// NamespacePrefix maps a space onto its namespace: <prefix><space>.
	NamespacePrefix string
	// Domain serves apps as <app>.<space>.<domain>.
	Domain string
	// Registry feeds the per-app image pull secret.
	Registry config.Registry
	// AuthAvailable says the installation has an IdP — without one there is
	// no "protected", only public and internal.
	AuthAvailable bool
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
	return &Runtime{c: client, opts: opts}
}

// Client exposes the underlying cluster client for Kubernetes-specific
// callers (the installer, the gateway) that need more than the Provider.
func (r *Runtime) Client() *Client { return r.c }

// ns is where a space lives: all its apps share the namespace.
func (r *Runtime) ns(space string) string { return r.opts.NamespacePrefix + space }

// --- Spaces ---------------------------------------------------------------

func (r *Runtime) ValidateSpace(space string) error {
	if !config.ValidName(space) {
		return fmt.Errorf("%q is not a valid space name — lowercase letters, digits and dashes (max 40 chars)", space)
	}
	// With the default prefix, the space "system" resolves to donkey-system —
	// a crafted (or innocently repo-named) space must never reach the control
	// plane's objects.
	if r.ns(space) == manifests.SystemNamespace {
		return fmt.Errorf("the space name %q is reserved for donkey itself — pick another (space: <name> in donkey.yaml)", space)
	}
	return nil
}

func (r *Runtime) CheckSpaceFree(ctx context.Context, space string) error {
	if err := r.ValidateSpace(space); err != nil {
		return err
	}
	return r.c.CheckNamespaceFree(ctx, r.ns(space))
}

func (r *Runtime) ListSpaces(ctx context.Context) ([]string, error) {
	refs, err := r.c.ListSpaces(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ref := range refs {
		space := ref.Space
		if space == "" {
			space = strings.TrimPrefix(ref.Namespace, r.opts.NamespacePrefix)
		}
		out = append(out, space)
	}
	return out, nil
}

func (r *Runtime) CreateSpace(ctx context.Context, space string) error {
	if err := r.ValidateSpace(space); err != nil {
		return err
	}
	return r.c.Apply(ctx, []k8sruntime.Object{manifests.Namespace(space, r.ns(space))})
}

func (r *Runtime) DeleteSpace(ctx context.Context, space string) error {
	if err := r.ValidateSpace(space); err != nil {
		return err
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
			space = strings.TrimPrefix(ref.Namespace, r.opts.NamespacePrefix)
		}
		out = append(out, rt.AppRef{Space: space, App: ref.App})
	}
	return out, nil
}

func (r *Runtime) Status(ctx context.Context, space, app string) (*rt.AppStatus, error) {
	return r.c.AppStatus(ctx, r.ns(space), app)
}

func (r *Runtime) HasData(ctx context.Context, space, app string) bool {
	return r.c.HasDataClaim(ctx, r.ns(space), app)
}

func (r *Runtime) Restart(ctx context.Context, space, app string) (bool, error) {
	return r.c.RestartApp(ctx, r.ns(space), app)
}

func (r *Runtime) Logs(ctx context.Context, space, app string, opts rt.LogOptions, out io.Writer) error {
	return r.c.StreamLogs(ctx, r.ns(space), app, opts, out)
}

func (r *Runtime) Evidence(ctx context.Context, space, app string) rt.Evidence {
	return r.c.RolloutEvidenceFor(ctx, r.ns(space), app)
}

// --- Env ------------------------------------------------------------------

func (r *Runtime) GetEnv(ctx context.Context, space, app string) (map[string]string, error) {
	return r.c.GetEnv(ctx, r.ns(space), app)
}

func (r *Runtime) SetEnv(ctx context.Context, space, app string, env map[string]string) error {
	return r.c.SetEnv(ctx, r.ns(space), space, app, env)
}

// --- App metadata ---------------------------------------------------------

func (r *Runtime) AppMeta(ctx context.Context, space, app, key string) (string, error) {
	return r.c.DeploymentAnnotation(ctx, r.ns(space), app, key)
}

func (r *Runtime) SetAppMeta(ctx context.Context, space, app, key string, value *string) error {
	return r.c.SetAnnotations(ctx, "Deployment", r.ns(space), app, map[string]*string{key: value})
}

// --- Services (addons) ----------------------------------------------------

func (r *Runtime) ServiceBinding(ctx context.Context, space, owner, name string) (map[string]string, error) {
	return r.c.GetSecretData(ctx, r.ns(space), addons.SecretName(owner, name))
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
	var binding map[string]string
	var version, storage string
	if spec.External {
		binding, err = addon.ExternalBinding(spec.URL)
		if err != nil {
			return nil, "", "", rt.UserError{Err: err}
		}
		objs := []k8sruntime.Object{
			manifests.Namespace(space, ns),
			bindingSecret(space, addons.SharedOwner, ns, spec.Name, binding),
		}
		if err := r.c.Apply(ctx, objs); err != nil {
			return nil, "", "", err
		}
	} else {
		// The service takes its plain name, reachable as e.g. "postgres" the
		// way siblings reach each other — so it must not seize a name an app
		// (or a differently-named service) already holds.
		if component, exists, err := r.c.DeploymentComponent(ctx, ns, spec.Name); err != nil {
			return nil, "", "", err
		} else if exists && component != addons.Component(spec.Name) {
			return nil, "", "", rt.UserError{Err: fmt.Errorf("the name %q is taken by an app in this space — pick another for the service", spec.Name)}
		}
		in, err := r.provisionService(ctx, space, ns, addons.SharedOwner, spec)
		if err != nil {
			return nil, "", "", err
		}
		binding, version, storage = addon.Binding(in), in.Version, in.Storage
	}
	// Re-creating an external service may change its address — the copies the
	// attached apps hold have to follow, and their pods restart to see the
	// new values. (A managed re-apply keeps host and credential, so the
	// copies come out identical and nothing restarts.)
	attached, err := r.c.ListAddonAttachments(ctx, ns, addons.Component(spec.Name))
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

// provisionService creates (or re-applies) one managed addon instance with
// its binding secret and waits until it serves.
func (r *Runtime) provisionService(ctx context.Context, space, ns, owner string, spec rt.ServiceSpec) (addons.Input, error) {
	addon, err := addons.Get(spec.Type)
	if err != nil {
		return addons.Input{}, err
	}
	in := addons.Input{
		App: owner, Space: space, Namespace: ns,
		Name: spec.Name, Version: spec.Version, Storage: spec.Storage,
		AIProxyURL: aiProxyURL(),
	}
	if in.Version == "" {
		in.Version = addon.DefaultVersion()
	}
	if in.Storage == "" {
		in.Storage = addon.DefaultStorage()
	}
	existing, err := r.c.GetSecretData(ctx, ns, addons.SecretName(owner, spec.Name))
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
		manifests.Namespace(space, ns),
		bindingSecret(space, owner, ns, in.Name, addon.Binding(in)),
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
	return in, r.c.WaitForRollout(ctx, ns, in.Name)
}

func (r *Runtime) AttachService(ctx context.Context, space, app, name string, binding map[string]string) (bool, error) {
	ns := r.ns(space)
	if err := r.c.Apply(ctx, []k8sruntime.Object{attachmentSecret(space, app, ns, name, binding)}); err != nil {
		return false, err
	}
	// An app that is already deployed picks the binding up on the spot; one
	// that isn't gets it wired by its first deploy.
	return r.c.SetAddonEnvFrom(ctx, ns, app, addons.SecretName(app, name), true)
}

func (r *Runtime) DetachService(ctx context.Context, space, app, name string) error {
	ns := r.ns(space)
	// Unwire before deleting: a pod template that still references the
	// binding secret would fail to start once the secret is gone.
	if _, err := r.c.SetAddonEnvFrom(ctx, ns, app, addons.SecretName(app, name), false); err != nil {
		return err
	}
	return r.c.DetachAddon(ctx, ns, app, addons.Component(name))
}

func (r *Runtime) RemoveService(ctx context.Context, space, name string, keepData bool) ([]string, error) {
	ns := r.ns(space)
	attached, err := r.c.ListAddonAttachments(ctx, ns, addons.Component(name))
	if err != nil {
		return nil, err
	}
	for _, app := range attached {
		if _, err := r.c.SetAddonEnvFrom(ctx, ns, app, addons.SecretName(app, name), false); err != nil {
			return nil, err
		}
		if err := r.c.DetachAddon(ctx, ns, app, addons.Component(name)); err != nil {
			return nil, err
		}
	}
	if err := r.c.DeleteComponent(ctx, ns, addons.SharedOwner, addons.Component(name), keepData); err != nil {
		return nil, err
	}
	return attached, nil
}

func (r *Runtime) ServiceAttachments(ctx context.Context, space, name string) ([]string, error) {
	return r.c.ListAddonAttachments(ctx, r.ns(space), addons.Component(name))
}

func (r *Runtime) ServiceEndpoint(ctx context.Context, space, name string) (string, int32, error) {
	return r.c.ServiceEndpoint(ctx, r.ns(space), name)
}

func (r *Runtime) ListServices(ctx context.Context, space, owner string) ([]rt.ServiceBindingInfo, error) {
	return r.c.ListAddonBindings(ctx, r.ns(space), owner)
}

func (r *Runtime) SharedServiceValues(ctx context.Context, key string) ([]string, error) {
	return r.c.SharedBindingValues(ctx, addons.SharedOwner, key)
}

// --- Files ----------------------------------------------------------------

func (r *Runtime) ListFiles(ctx context.Context, space, app, path string) ([]rt.FileEntry, error) {
	return r.c.ListFiles(ctx, r.ns(space), app, path)
}

func (r *Runtime) StorageFolders(ctx context.Context, space, app string) ([]rt.StorageFolder, error) {
	return r.c.StorageFolders(ctx, r.ns(space), app)
}

func (r *Runtime) ReadFile(ctx context.Context, space, app, path string, out io.Writer) error {
	return r.c.ReadFile(ctx, r.ns(space), app, path, out)
}

func (r *Runtime) WriteFile(ctx context.Context, space, app, path string, in io.Reader) error {
	return r.c.WriteFile(ctx, r.ns(space), app, path, in)
}

func (r *Runtime) RemoveFile(ctx context.Context, space, app, path string) error {
	return r.c.RemoveFile(ctx, r.ns(space), app, path)
}

func (r *Runtime) MoveFile(ctx context.Context, space, app, from, to string) error {
	return r.c.MoveFile(ctx, r.ns(space), app, from, to)
}

func (r *Runtime) MakeDir(ctx context.Context, space, app, path string) error {
	return r.c.MakeDir(ctx, r.ns(space), app, path)
}

// --- Health ---------------------------------------------------------------

func (r *Runtime) Checks(ctx context.Context) []rt.Check {
	var checks []rt.Check

	// Every app rides through the donkey gateway — it is the edge, so if it
	// is down, so is every app, which is exactly what doctor exists to say
	// plainly.
	dg := rt.Check{Name: "App gateway (donkey-gateway)"}
	gwURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/healthz",
		manifests.GatewayHealthServiceName, manifests.SystemNamespace, manifests.GatewayHealthServicePort)
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
	if ok, err := r.c.HasDefaultStorageClass(ctx); err != nil {
		sc.Detail = err.Error()
	} else if !ok {
		sc.Detail = "no default StorageClass — apps with storage and provisioned addons won't get disks"
	} else {
		sc.OK = true
		sc.Detail = "available"
	}
	checks = append(checks, sc)

	return checks
}
