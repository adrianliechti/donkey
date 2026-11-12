package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

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

func (r *Runtime) pluginInstance(space, name, offering string) *addonv1.Instance {
	return &addonv1.Instance{
		Installation: r.opts.SystemNamespace, Space: space, Name: name, Offering: offering,
		Kubernetes: &addonv1.KubernetesContext{
			Namespace:      r.ns(space),
			ConsumerLabels: manifests.SpacePodSelector(r.opts.SystemNamespace, space),
		},
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
	if i := state.Instance; i == nil || i.Installation != r.opts.SystemNamespace || i.Space != space || i.Name != name || i.GetKubernetes().GetNamespace() != r.ns(space) || state.Type == "" || i.Offering != state.Type {
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

func (r *Runtime) savePluginState(ctx context.Context, state *addons.Provisioning) error {
	secret, err := r.pluginStateSecret(state)
	if err != nil {
		return err
	}
	return r.c.Apply(ctx, []k8sruntime.Object{secret})
}

func (r *Runtime) provisionPlugin(ctx context.Context, space string, spec rt.ServiceSpec, plugin *addons.Plugin, desc *addonv1.Offering, previous *addons.Provisioning) (map[string]string, addons.Parameters, error) {
	if _, exists, err := r.c.DeploymentIdentity(ctx, r.ns(space), r.name(space, spec.Name)); err != nil {
		return nil, nil, err
	} else if exists {
		return nil, nil, fmt.Errorf("the name %q is already taken by a local workload", spec.Name)
	}
	state := previous
	if state == nil {
		// Refuse to replace an existing local or external service with a
		// plugin: otherwise its backing data would lose its lifecycle owner.
		binding, err := r.ServiceBinding(ctx, space, addons.SharedOwner, spec.Name)
		if err != nil {
			return nil, nil, err
		}
		if len(binding) != 0 {
			return nil, nil, fmt.Errorf("remove service %s before changing its provisioner", spec.Name)
		}
		state = &addons.Provisioning{Type: spec.Type, Provider: plugin.Provider(), Instance: r.pluginInstance(space, spec.Name, spec.Type)}
	}
	params, reason, err := addons.PlanProvision(desc, spec.Parameters, previous)
	if err != nil {
		return nil, nil, rt.UserError{Err: err}
	}
	configuration, err := rt.SpaceConfiguration(ctx, r, space, desc, previous)
	if err != nil {
		return nil, nil, err
	}
	request := &addonv1.ProvisionRequest{Instance: state.Instance, Parameters: params.Struct(), PreviousParameters: state.Parameters.Struct(), Reason: reason,
		SpaceConfiguration: configuration.Struct(), PreviousSpaceConfiguration: state.SpaceConfiguration.Struct()}
	state.SpaceConfiguration = configuration
	// Intent first: even a lost response leaves an identity, the offering
	// and the parameters to retry or remove with.
	state.Parameters, state.Offering, state.Protocol, state.Error = params, desc, addons.NormalizeProtocol(desc.Protocol), ""
	state.Pending = true
	intent, err := r.pluginStateSecret(state)
	if err != nil {
		return nil, nil, err
	}
	if err := r.c.Apply(ctx, append(r.spaceObjects(space), intent)); err != nil {
		return nil, nil, err
	}
	info, err := plugin.Provision(ctx, request, nil)
	if err != nil {
		state.Error = failureMessage(err)
		if saveErr := r.savePluginState(ctx, state); saveErr != nil {
			log.Printf("service %s/%s: recording provisioning failure: %v", space, spec.Name, saveErr)
		}
		return nil, nil, err
	}
	if info.Parameters != nil {
		params = addons.ParametersFrom(info.Parameters)
	}
	state.Info, state.Parameters, state.Stopped, state.Error = info, params, false, ""
	state.Pending = false
	// Retain the successful response before policy validation or application.
	// If either fails, reconciliation and removal still have the destinations
	// and credentials returned by the provisioner, not merely the intent.
	if err := r.savePluginState(ctx, state); err != nil {
		return nil, nil, err
	}
	var objects []k8sruntime.Object
	if r.opts.IsolateSpaces && r.opts.RestrictEgress {
		policy, err := manifests.AddonEgressPolicy(space, r.ns(space), r.opts.SystemNamespace, spec.Name, r.name(space, spec.Name), info)
		if err != nil {
			state.Error = failureMessage(err)
			_ = r.savePluginState(ctx, state)
			return nil, nil, err
		}
		objects = append(objects, policy)
	} else if err := r.deletePluginEgress(ctx, space, spec.Name); err != nil {
		return nil, nil, err
	}
	objects = append(objects, spaceBindingSecret(space, r.name(space, spec.Name), r.ns(space), spec.Name, info.Env,
		addons.Metadata{Type: plugin.Type(), Protocol: state.Protocol, Parameters: params}))
	if err := r.c.Apply(ctx, objects); err != nil {
		return nil, nil, err
	}
	return info.Env, params, nil
}

// failureMessage keeps a provisioning error short enough for a listing.
func failureMessage(err error) string {
	message := err.Error()
	if len(message) > 500 {
		message = message[:500] + "…"
	}
	return message
}

func (r *Runtime) deletePluginEgress(ctx context.Context, space, name string) error {
	return r.c.deleteKindsBySelector(ctx, r.ns(space), []string{"NetworkPolicy"},
		manifests.AddonLabels(space, name, manifests.RoleProvisioning), false)
}

// deprovisionPlugin asks the plugin to remove (or stop) the instance. With
// force, a plugin that is gone or failing no longer blocks: the record is
// dropped and the backing service left to the operator.
func (r *Runtime) deprovisionPlugin(ctx context.Context, space, name string, keepData, force bool) error {
	if keepData && force {
		return rt.UserError{Err: fmt.Errorf("--orphan cannot be combined with --keep-data")}
	}
	state, err := r.pluginState(ctx, space, name)
	if err != nil {
		return err
	}
	if state == nil {
		if force {
			return rt.UserError{Err: fmt.Errorf("--orphan requires a plugin-provided service")}
		}
		return nil
	}
	if force {
		log.Printf("service %s/%s: orphaning plugin resources", space, name)
	} else {
		plugin, err := state.Plugin()
		if err == nil {
			err = plugin.Deprovision(ctx, state.Instance, keepData)
		}
		if err != nil {
			return fmt.Errorf("%w — remove with --orphan to drop donkey's record and leave the backing service behind", err)
		}
	}
	if err := r.deletePluginEgress(ctx, space, name); err != nil {
		return err
	}
	if keepData {
		state.Stopped = true
		if err := r.savePluginState(ctx, state); err != nil {
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

// unboundPluginServices lists plugin instances that have no binding right
// now — pending, failed or stopped — so the console can show and remove them.
func (r *Runtime) unboundPluginServices(ctx context.Context, space string, bound map[string]bool) ([]rt.ServiceBindingInfo, error) {
	names, err := r.pluginServiceNames(ctx, space)
	if err != nil {
		return nil, err
	}
	var out []rt.ServiceBindingInfo
	for _, name := range names {
		if bound[name] {
			continue
		}
		state, err := r.pluginState(ctx, space, name)
		if err != nil {
			return nil, err
		}
		if state == nil {
			continue
		}
		out = append(out, rt.ServiceBindingInfo{
			Name: name, Type: state.Type, Protocol: state.Protocol, Parameters: state.Parameters,
			State: state.State(), Message: state.Error,
		})
	}
	return out, nil
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
