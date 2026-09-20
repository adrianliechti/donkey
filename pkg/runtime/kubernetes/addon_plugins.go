package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/adrianliechti/donkey/internal/addons"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

const provisioningKey = "provisioning.json"

func (r *Runtime) pluginInstance(space, name string) *addonv1.Instance {
	return &addonv1.Instance{
		Installation: r.opts.SystemNamespace, Space: space, Name: name,
		Runtime: "kubernetes", Namespace: r.ns(space),
		ConsumerLabels: manifests.SpacePodSelector(r.opts.SystemNamespace, space),
	}
}

func (r *Runtime) pluginState(ctx context.Context, space, name string) (*addons.Provisioning, error) {
	secret, err := r.c.Clientset.CoreV1().Secrets(r.ns(space)).Get(ctx, r.name(space, name)+"-provisioning", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := r.c.requireOwnedObject("Secret", secret.Namespace, secret.Name, secret.Labels,
		manifests.AddonLabels(space, name, manifests.RoleProvisioning)); err != nil {
		return nil, err
	}
	var state addons.Provisioning
	if err := json.Unmarshal(secret.Data[provisioningKey], &state); err != nil {
		return nil, fmt.Errorf("service %s has unreadable provisioning state", name)
	}
	if i := state.Instance; i == nil || i.Installation != r.opts.SystemNamespace || i.Space != space || i.Name != name || i.Namespace != r.ns(space) || i.Runtime != "kubernetes" || state.Type == "" {
		return nil, fmt.Errorf("service %s has mismatched provisioning identity", name)
	}
	return &state, nil
}

func (r *Runtime) pluginStateSecret(state *addons.Provisioning) (*corev1.Secret, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	i := state.Instance
	return bindingSecret(r.name(i.Space, i.Name)+"-provisioning", r.ns(i.Space),
		manifests.AddonLabels(i.Space, i.Name, manifests.RoleProvisioning),
		map[string]string{provisioningKey: string(data)}), nil
}

func (r *Runtime) provisionPlugin(ctx context.Context, space string, spec rt.ServiceSpec, plugin *addons.Plugin, previous *addons.Provisioning) (map[string]string, string, string, error) {
	if _, exists, err := r.c.DeploymentIdentity(ctx, r.ns(space), r.name(space, spec.Name)); err != nil {
		return nil, "", "", err
	} else if exists {
		return nil, "", "", fmt.Errorf("the name %q is already taken by a local workload", spec.Name)
	}
	version, storage := spec.Version, spec.Storage
	if version == "" {
		version = plugin.DefaultVersion()
	}
	if storage == "" {
		storage = plugin.DefaultStorage()
	}
	state := previous
	if state == nil {
		// Refuse to replace an existing local or external service with a
		// plugin: otherwise its backing data would lose its lifecycle owner.
		binding, err := r.ServiceBinding(ctx, space, addons.SharedOwner, spec.Name)
		if err != nil {
			return nil, "", "", err
		}
		if len(binding) != 0 {
			return nil, "", "", fmt.Errorf("remove service %s before changing its provisioner", spec.Name)
		}
		state = &addons.Provisioning{Type: spec.Type, Instance: r.pluginInstance(space, spec.Name)}
	}
	intent, err := r.pluginStateSecret(state)
	if err != nil {
		return nil, "", "", err
	}
	if err := r.c.Apply(ctx, append(r.spaceObjects(space), intent)); err != nil {
		return nil, "", "", err
	}
	info, err := plugin.Provision(ctx, &addonv1.ProvisionRequest{Instance: state.Instance, Version: version, Storage: storage})
	if err != nil {
		return nil, "", "", err
	}
	state.Info, state.Stopped = info, false
	secret, err := r.pluginStateSecret(state)
	if err != nil {
		return nil, "", "", err
	}
	objects := []k8sruntime.Object{secret}
	if r.opts.IsolateSpaces && r.opts.RestrictEgress {
		policy, err := manifests.AddonEgressPolicy(space, r.ns(space), r.opts.SystemNamespace, spec.Name, r.name(space, spec.Name), info)
		if err != nil {
			return nil, "", "", err
		}
		objects = append(objects, policy)
	} else if err := r.deletePluginEgress(ctx, space, spec.Name); err != nil {
		return nil, "", "", err
	}
	objects = append(objects, spaceBindingSecret(space, r.name(space, spec.Name), r.ns(space), spec.Name, info.Env,
		addons.Metadata{Type: plugin.Type(), Protocol: info.Protocol}))
	if err := r.c.Apply(ctx, objects); err != nil {
		return nil, "", "", err
	}
	return info.Env, version, storage, nil
}

func (r *Runtime) deletePluginEgress(ctx context.Context, space, name string) error {
	return r.c.deleteKindsBySelector(ctx, r.ns(space), []string{"NetworkPolicy"},
		manifests.AddonLabels(space, name, manifests.RoleProvisioning), false)
}

func (r *Runtime) deprovisionPlugin(ctx context.Context, space, name string, keepData bool) error {
	state, err := r.pluginState(ctx, space, name)
	if err != nil || state == nil {
		return err
	}
	plugin, err := state.Plugin()
	if err != nil {
		return err
	}
	if err := plugin.Deprovision(ctx, state.Instance, keepData); err != nil {
		return err
	}
	if err := r.deletePluginEgress(ctx, space, name); err != nil {
		return err
	}
	if keepData {
		state.Stopped = true
		secret, err := r.pluginStateSecret(state)
		if err != nil {
			return err
		}
		if err := r.c.Apply(ctx, []k8sruntime.Object{secret}); err != nil {
			return err
		}
		// The plugin retains credentials with its data. Retire the active
		// binding so a later addon add/deploy calls Provision to resume it.
		return r.c.DeleteObject(ctx, "Secret", r.ns(space), r.spaceBindingName(space, name),
			manifests.AddonLabels(space, name, manifests.RoleBinding))
	}
	// The normal addon/space deletion removes state last. Keep it here so
	// a failed cleanup still knows which plugin to call on retry.
	return nil
}

func (r *Runtime) pluginServiceNames(ctx context.Context, space string) ([]string, error) {
	secrets, err := r.c.Clientset.CoreV1().Secrets(r.ns(space)).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set(map[string]string{
			manifests.LabelInstallation: r.opts.SystemNamespace,
			manifests.LabelSpace:        space, manifests.LabelRole: manifests.RoleProvisioning,
		}).String(),
	})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, s := range secrets.Items {
		if name := s.Labels[manifests.LabelAddon]; name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// Reconcile after installation changes too: enabling a fence must retain
// existing plugin connections, and disabling it must remove additive rules.
func (r *Runtime) reconcilePluginEgress(ctx context.Context, space string) error {
	names, err := r.pluginServiceNames(ctx, space)
	if err != nil {
		return err
	}
	for _, name := range names {
		state, err := r.pluginState(ctx, space, name)
		if err != nil {
			return err
		}
		if state == nil {
			continue
		}
		if r.opts.IsolateSpaces && r.opts.RestrictEgress && !state.Stopped && state.Info != nil {
			policy, err := manifests.AddonEgressPolicy(space, r.ns(space), r.opts.SystemNamespace, name, r.name(space, name), state.Info)
			if err != nil {
				return err
			}
			if err := r.c.Apply(ctx, []k8sruntime.Object{policy}); err != nil {
				return err
			}
		} else if err := r.deletePluginEgress(ctx, space, name); err != nil {
			return err
		}
	}
	return nil
}
