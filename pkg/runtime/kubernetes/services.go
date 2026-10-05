package kubernetes

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// --- Services (addons) ----------------------------------------------------

func (r *Runtime) ServiceBinding(ctx context.Context, space, owner, name string) (map[string]string, error) {
	identity := manifests.AddonLabels(space, name, manifests.RoleBinding)
	if owner == addons.SharedOwner {
		return r.c.GetOwnedSecretData(ctx, r.ns(space), r.spaceBindingName(space, name), identity)
	}
	identity[manifests.LabelApp] = owner
	return r.c.GetOwnedSecretData(ctx, r.ns(space), manifests.AppBindingSecretName(r.name(space, owner), name), identity)
}

func (r *Runtime) ServiceConnection(ctx context.Context, space, name string) (rt.ServiceConnection, error) {
	ns, object := r.ns(space), r.spaceBindingName(space, name)
	secret, err := r.c.Clientset.CoreV1().Secrets(ns).Get(ctx, object, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return rt.ServiceConnection{}, nil
	}
	if err != nil {
		return rt.ServiceConnection{}, err
	}
	if err := r.c.requireOwnedObject("Secret", ns, object, secret.Labels, manifests.AddonLabels(space, name, manifests.RoleBinding)); err != nil {
		return rt.ServiceConnection{}, err
	}
	metadata := bindingMetadata(secret)
	return rt.ServiceConnection{Binding: secretStringData(secret), Type: metadata.Type, Protocol: metadata.Protocol, Parameters: metadata.Parameters}, nil
}

// EnsureService creates (or re-applies) one service of the space —
// provisioned in-cluster, or external as a binding-only Secret — and returns
// its binding values.
func (r *Runtime) EnsureService(ctx context.Context, space string, spec rt.ServiceSpec) (map[string]string, map[string]any, error) {
	ns := r.ns(space)
	if err := config.ValidateServiceName(spec.Name); err != nil {
		return nil, nil, rt.UserError{Err: err}
	}
	if err := r.opts.Placement.ValidateNames(space, spec.Name); err != nil {
		return nil, nil, rt.UserError{Err: err}
	}
	// The record comes first: an instance a provider created resolves
	// through its recorded offering when the provider is not answering.
	previous, err := r.pluginState(ctx, space, spec.Name)
	if err != nil {
		return nil, nil, err
	}
	if previous != nil && previous.Type != spec.Type {
		return nil, nil, fmt.Errorf("remove service %s before changing its provisioner", spec.Name)
	}
	addon, err := addons.Lookup(ctx, spec.Type, previous)
	if err != nil {
		return nil, nil, rt.UserError{Err: err}
	}
	desc, err := addon.Describe(ctx)
	if err != nil {
		return nil, nil, err
	}
	connection, err := r.ServiceConnection(ctx, space, spec.Name)
	if err != nil {
		return nil, nil, err
	}
	if connection.Type != "" && connection.Type != spec.Type {
		return nil, nil, rt.UserError{Err: fmt.Errorf("remove service %s before changing its offering", spec.Name)}
	}
	spec.Parameters, err = rt.PrepareService(ctx, r, r.opts.CommonOfferings, space, desc, spec.Parameters, previous != nil || len(connection.Binding) > 0)
	if err != nil {
		return nil, nil, err
	}
	if err := r.ensureSpaceHome(ctx, space); err != nil {
		return nil, nil, err
	}
	plugin, isPlugin := addon.(*addons.Plugin)
	builtin, _ := addon.(addons.Builtin)
	if previous != nil && (!isPlugin || spec.External) {
		return nil, nil, fmt.Errorf("remove service %s before changing its provisioner", spec.Name)
	}
	if isPlugin && spec.External {
		return nil, nil, rt.UserError{Err: fmt.Errorf("the %s service is provisioned by provider %s; external URL bindings are not supported for it", spec.Type, plugin.Provider())}
	}
	var binding map[string]string
	var params addons.Parameters
	switch {
	case isPlugin && !spec.External:
		binding, params, err = r.provisionPlugin(ctx, space, spec, plugin, desc, previous)
		if err != nil {
			return nil, nil, err
		}
	case spec.External:
		binding, err = builtin.ExternalBinding(spec.URL)
		if err != nil {
			return nil, nil, rt.UserError{Err: err}
		}
		objs := append(r.spaceObjects(space),
			spaceBindingSecret(space, r.name(space, spec.Name), ns, spec.Name, binding, addons.MetadataFor(desc, nil)))
		if err := r.c.Apply(ctx, objs); err != nil {
			return nil, nil, err
		}
	default:
		// The service takes its plain name, reachable as e.g. "postgres" the
		// way siblings reach each other — so it must not seize a name an app
		// (or a differently-named service) already holds.
		if identity, exists, err := r.c.DeploymentIdentity(ctx, ns, r.name(space, spec.Name)); err != nil {
			return nil, nil, err
		} else if exists {
			expected := manifests.AddonLabels(space, spec.Name, manifests.RoleAddon)
			if err := r.c.requireOwnedObject("Deployment", ns, r.name(space, spec.Name), identity, expected); err != nil {
				if r.c.requireOwnedObject("Deployment", ns, r.name(space, spec.Name), identity,
					manifests.Labels(space, spec.Name, manifests.RoleWorkload)) == nil {
					return nil, nil, rt.UserError{Err: fmt.Errorf("the name %q is taken by an app in this space — pick another for the service", spec.Name)}
				}
				return nil, nil, err
			}
		}
		in, err := r.provisionService(ctx, space, ns, spec, builtin, desc)
		if err != nil {
			return nil, nil, err
		}
		binding, params = builtin.Binding(in), in.Parameters
	}
	// Re-creating an external service may change its address — the copies the
	// attached apps hold have to follow, and their pods restart to see the
	// new values. (A managed re-apply keeps host and credential, so the
	// copies come out identical and nothing restarts.)
	attached, err := r.c.ListAddonAttachments(ctx, ns, space, spec.Name)
	if err != nil {
		return nil, nil, err
	}
	for _, app := range attached {
		obj := r.name(space, app)
		if err := r.c.Apply(ctx, []k8sruntime.Object{attachmentSecret(space, app, obj, ns, spec.Name, binding)}); err != nil {
			return nil, nil, err
		}
		if spec.External || isPlugin {
			if _, err := r.c.RestartApp(ctx, ns, obj, space, app); err != nil {
				return nil, nil, err
			}
		}
	}
	return binding, params, nil
}

// provisionService creates (or re-applies) one space-owned addon instance
// with its canonical binding secret, and waits until it serves.
func (r *Runtime) provisionService(ctx context.Context, space, ns string, spec rt.ServiceSpec, addon addons.Builtin, desc *addonv1.Offering) (addons.Input, error) {
	// An existing instance keeps its credential and, unless asked
	// otherwise, its parameters.
	existing, err := r.ServiceConnection(ctx, space, spec.Name)
	if err != nil {
		return addons.Input{}, err
	}
	params, err := addons.ResolveInstanceParameters(desc, spec.Parameters, existing.Parameters)
	if err != nil {
		return addons.Input{}, rt.UserError{Err: err}
	}
	in := addons.Input{
		Space: space, Namespace: ns,
		Name: spec.Name, ObjectName: r.name(space, spec.Name),
		ImageRepository: r.opts.AddonImages.Repository(spec.Type),
		AIProxyURL:      r.aiProxyURL(),
	}.WithParameters(params)
	// Reuse the credential the instance was initialized with — rotating it
	// here would lock the service out of its own kept data.
	if pw, ok := existing.Binding[addon.CredentialKey()]; ok {
		in.Password = pw
	} else if in.Password, err = addons.NewPassword(); err != nil {
		return in, err
	}
	objs := append(r.spaceObjects(space),
		spaceBindingSecret(space, in.HostName(), ns, in.Name, addon.Binding(in), addons.MetadataFor(desc, params)))
	workload, err := addon.Workload(in)
	if err != nil {
		return in, err
	}
	if workload != nil {
		var pull *imagePullCredentials
		if len(r.opts.AddonImagePullSecrets) == 0 {
			pull, err = r.addonImagePullCredentials(workload.Image)
			if err != nil {
				return in, err
			}
		}
		addonObjs, err := addonObjects(in, workload, pull, r.opts.AddonImagePullSecrets)
		if err != nil {
			return in, err
		}
		objs = append(objs, addonObjs...)
	}
	if err := r.c.preserveDataClaims(ctx, objs); err != nil {
		return in, err
	}
	if err := r.c.Apply(ctx, objs); err != nil {
		return in, err
	}
	// A binding-only service (like openai) runs nothing — there is no
	// rollout to wait for.
	if workload == nil {
		return in, nil
	}
	return in, r.c.WaitForRollout(ctx, ns, in.HostName(),
		manifests.AddonLabels(space, spec.Name, manifests.RoleAddon))
}

// addonImagePullCredentials reuses the configured registry login when an
// addon mirror lives on the same enterprise registry host. Credentials are
// never attached to a public image or to an unrelated mirror host.
func (r *Runtime) addonImagePullCredentials(image string) (*imagePullCredentials, error) {
	host := r.opts.Registry.Host()
	if host == "" || !strings.HasPrefix(image, host+"/") {
		return nil, nil
	}
	username, password, err := r.opts.Registry.Credentials()
	if err != nil {
		return nil, err
	}
	if username == "" || password == "" {
		return nil, nil
	}
	return &imagePullCredentials{Host: host, Username: username, Password: password}, nil
}

func (r *Runtime) AttachService(ctx context.Context, space, app, name string, binding map[string]string) (bool, error) {
	if err := rt.CheckServiceAttachment(ctx, r, r.opts.CommonOfferings, space, app, name); err != nil {
		return false, err
	}
	// Writes an attachment secret named from the pair — same rule as SetEnv.
	if err := r.ValidateApp(space, app); err != nil {
		return false, rt.UserError{Err: err}
	}
	ns := r.ns(space)
	obj := r.name(space, app)
	if err := r.c.Apply(ctx, []k8sruntime.Object{attachmentSecret(space, app, obj, ns, name, binding)}); err != nil {
		return false, err
	}
	// An app that is already deployed picks the binding up on the spot; one
	// that isn't gets it wired by its first deploy.
	return r.c.SetAddonEnvFrom(ctx, ns, obj, space, app, manifests.AppBindingSecretName(obj, name), true)
}

func (r *Runtime) DetachService(ctx context.Context, space, app, name string) error {
	ns := r.ns(space)
	obj := r.name(space, app)
	// Unwire before deleting: a pod template that still references the
	// binding secret would fail to start once the secret is gone.
	if _, err := r.c.SetAddonEnvFrom(ctx, ns, obj, space, app, manifests.AppBindingSecretName(obj, name), false); err != nil {
		return err
	}
	return r.c.DetachAddon(ctx, ns, space, app, name)
}

func (r *Runtime) RemoveService(ctx context.Context, space, name string, keepData, force bool) ([]string, error) {
	ns := r.ns(space)
	attached, err := r.c.ListAddonAttachments(ctx, ns, space, name)
	if err != nil {
		return nil, err
	}
	if err := r.deprovisionPlugin(ctx, space, name, keepData, force); err != nil {
		return nil, err
	}
	var apps []string
	for _, obj := range attached {
		objectName := r.name(space, obj)
		if _, err := r.c.SetAddonEnvFrom(ctx, ns, objectName, space, obj, manifests.AppBindingSecretName(objectName, name), false); err != nil {
			return nil, err
		}
		if err := r.c.DetachAddon(ctx, ns, space, obj, name); err != nil {
			return nil, err
		}
		apps = append(apps, obj)
	}
	if err := r.c.DeleteAddon(ctx, ns, space, name, keepData); err != nil {
		return nil, err
	}
	return apps, nil
}

func (r *Runtime) ServiceAttachments(ctx context.Context, space, name string) ([]string, error) {
	attached, err := r.c.ListAddonAttachments(ctx, r.ns(space), space, name)
	if err != nil {
		return nil, err
	}
	return attached, nil
}

func (r *Runtime) ServiceEndpoint(ctx context.Context, space, name string) (string, int32, error) {
	state, err := r.pluginState(ctx, space, name)
	if err != nil {
		return "", 0, err
	}
	if state != nil {
		host, port := state.Endpoint()
		return host, port, nil
	}
	return r.c.ServiceEndpoint(ctx, r.ns(space), r.name(space, name), space, name)
}

func (r *Runtime) ListServices(ctx context.Context, space, owner string) ([]rt.ServiceBindingInfo, error) {
	app := owner
	if owner == addons.SharedOwner {
		app = ""
	}
	bindings, err := r.c.ListAddonBindings(ctx, r.ns(space), space, app)
	if err != nil {
		return nil, err
	}
	if app == "" {
		// The space's own view also shows plugin instances that have no
		// binding yet (or any more): pending, failed and stopped ones.
		bound := map[string]bool{}
		for _, b := range bindings {
			bound[b.Name] = true
		}
		unbound, err := r.unboundPluginServices(ctx, space, bound)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, unbound...)
		sort.Slice(bindings, func(i, j int) bool { return bindings[i].Name < bindings[j].Name })
		return bindings, nil
	}
	shared, err := r.c.ListAddonBindings(ctx, r.ns(space), space, "")
	if err != nil {
		return nil, err
	}
	metadata := make(map[string]rt.ServiceBindingInfo, len(shared))
	for _, service := range shared {
		metadata[service.Name] = service
	}
	for i := range bindings {
		if service, ok := metadata[bindings[i].Name]; ok {
			bindings[i].Type, bindings[i].Protocol, bindings[i].Parameters = service.Type, service.Protocol, service.Parameters
		}
	}
	return bindings, nil
}

func (r *Runtime) AIProxyTokens(ctx context.Context) ([]string, error) {
	return r.c.SharedBindingValues(ctx, "OPENAI_API_KEY", "OPENAI_BASE_URL", r.aiProxyURL())
}

// aiProxyURL is where apps reach this installation's AI endpoint — its own,
// not whichever one answers first. donkey-ai rather than donkey-server: the
// address apps are handed must not be the process that writes to the cluster.
//
// ".svc" and no further, deliberately. The cluster domain is a per-cluster
// setting ("cluster.local" is only the default), and every pod's resolv.conf
// carries "svc.<domain>" in its search list — so the short form resolves
// where a hardcoded fully-qualified one would not.
func (r *Runtime) aiProxyURL() string {
	return fmt.Sprintf("http://%s.%s.svc/v1", manifests.AIAppName, r.opts.SystemNamespace)
}
