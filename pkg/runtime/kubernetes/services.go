package kubernetes

import (
	"context"
	"fmt"
	"strings"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// --- Services (addons) ----------------------------------------------------

func (r *Runtime) ServiceBinding(ctx context.Context, space, owner, name string) (map[string]string, error) {
	identity := manifests.AddonLabels(space, name, manifests.RoleBinding)
	if owner != addons.SharedOwner {
		identity[manifests.LabelApp] = owner
	}
	return r.c.GetOwnedSecretData(ctx, r.ns(space), addons.SecretName(r.name(space, owner), name), identity)
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
		objs := append(r.spaceObjects(space),
			spaceBindingSecret(space, r.sharedOwner(space), ns, spec.Name, binding))
		if err := r.c.Apply(ctx, objs); err != nil {
			return nil, "", "", err
		}
	} else {
		// The service takes its plain name, reachable as e.g. "postgres" the
		// way siblings reach each other — so it must not seize a name an app
		// (or a differently-named service) already holds.
		if identity, exists, err := r.c.DeploymentIdentity(ctx, ns, r.name(space, spec.Name)); err != nil {
			return nil, "", "", err
		} else if exists {
			expected := manifests.AddonLabels(space, spec.Name, manifests.RoleAddon)
			if err := r.c.requireOwnedObject("Deployment", ns, r.name(space, spec.Name), identity, expected); err != nil {
				if r.c.requireOwnedObject("Deployment", ns, r.name(space, spec.Name), identity,
					manifests.Labels(space, spec.Name, manifests.RoleWorkload)) == nil {
					return nil, "", "", rt.UserError{Err: fmt.Errorf("the name %q is taken by an app in this space — pick another for the service", spec.Name)}
				}
				return nil, "", "", err
			}
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
	attached, err := r.c.ListAddonAttachments(ctx, ns, space, spec.Name)
	if err != nil {
		return nil, "", "", err
	}
	for _, app := range attached {
		obj := r.name(space, app)
		if err := r.c.Apply(ctx, []k8sruntime.Object{attachmentSecret(space, app, obj, ns, spec.Name, binding)}); err != nil {
			return nil, "", "", err
		}
		if spec.External {
			if _, err := r.c.RestartApp(ctx, ns, obj, space, app); err != nil {
				return nil, "", "", err
			}
		}
	}
	return binding, version, storage, nil
}

// provisionService creates (or re-applies) one space-owned addon instance
// with its canonical binding secret, and waits until it serves.
func (r *Runtime) provisionService(ctx context.Context, space, ns string, spec rt.ServiceSpec) (addons.Input, error) {
	addon, err := addons.Get(spec.Type)
	if err != nil {
		return addons.Input{}, err
	}
	in := addons.Input{
		App: r.sharedOwner(space), Space: space, Namespace: ns,
		Name: spec.Name, ObjectName: r.name(space, spec.Name),
		Version: spec.Version, Storage: spec.Storage,
		ImageRepository: r.opts.AddonImages.Repository(spec.Type),
		AIProxyURL:      r.aiProxyURL(),
	}
	if in.Version == "" {
		in.Version = addon.DefaultVersion()
	}
	if in.Storage == "" {
		in.Storage = addon.DefaultStorage()
	}
	existing, err := r.c.GetOwnedSecretData(ctx, ns, addons.SecretName(in.App, spec.Name),
		manifests.AddonLabels(space, spec.Name, manifests.RoleBinding))
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
	objs := append(r.spaceObjects(space),
		spaceBindingSecret(space, in.App, ns, in.Name, addon.Binding(in)))
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
	return r.c.SetAddonEnvFrom(ctx, ns, obj, space, app, addons.SecretName(obj, name), true)
}

func (r *Runtime) DetachService(ctx context.Context, space, app, name string) error {
	ns := r.ns(space)
	obj := r.name(space, app)
	// Unwire before deleting: a pod template that still references the
	// binding secret would fail to start once the secret is gone.
	if _, err := r.c.SetAddonEnvFrom(ctx, ns, obj, space, app, addons.SecretName(obj, name), false); err != nil {
		return err
	}
	return r.c.DetachAddon(ctx, ns, space, app, name)
}

func (r *Runtime) RemoveService(ctx context.Context, space, name string, keepData bool) ([]string, error) {
	ns := r.ns(space)
	attached, err := r.c.ListAddonAttachments(ctx, ns, space, name)
	if err != nil {
		return nil, err
	}
	var apps []string
	for _, obj := range attached {
		objectName := r.name(space, obj)
		if _, err := r.c.SetAddonEnvFrom(ctx, ns, objectName, space, obj, addons.SecretName(objectName, name), false); err != nil {
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
	return r.c.ServiceEndpoint(ctx, r.ns(space), r.name(space, name), space, name)
}

func (r *Runtime) ListServices(ctx context.Context, space, owner string) ([]rt.ServiceBindingInfo, error) {
	app := owner
	if owner == addons.SharedOwner {
		app = ""
	}
	return r.c.ListAddonBindings(ctx, r.ns(space), space, app)
}

func (r *Runtime) SharedServiceValues(ctx context.Context, key string) ([]string, error) {
	return r.c.SharedBindingValues(ctx, key)
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
