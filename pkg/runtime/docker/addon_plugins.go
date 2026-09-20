package docker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"

	cerrdefs "github.com/containerd/errdefs"

	"github.com/adrianliechti/donkey/internal/addons"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
)

func (r *Runtime) pluginState(space, name string) (*addons.Provisioning, error) {
	var state addons.Provisioning
	found, err := r.state.readJSON(&state, "provisioning", space, name+".json")
	if err != nil || !found {
		return nil, err
	}
	i := state.Instance
	if i == nil || i.Installation != dockerInstallation || i.Space != space || i.Name != name || i.Runtime != "docker" || i.Namespace != netName(space) || state.Type == "" {
		return nil, fmt.Errorf("service %s has mismatched provisioning identity", name)
	}
	return &state, nil
}

func (r *Runtime) savePluginState(space string, state *addons.Provisioning) error {
	return r.state.writeJSON(state, "provisioning", space, state.Instance.Name+".json")
}

func (r *Runtime) provisionPlugin(ctx context.Context, space string, spec rt.ServiceSpec, plugin *addons.Plugin, desc *addonv1.DescribeResponse, previous *addons.Provisioning) (map[string]string, addons.Parameters, error) {
	if _, err := r.cli.ContainerInspect(ctx, ctrName(space, spec.Name)); err == nil {
		return nil, nil, fmt.Errorf("the name %q is already taken by a local workload", spec.Name)
	} else if !cerrdefs.IsNotFound(err) {
		return nil, nil, err
	}
	state := previous
	if state == nil {
		binding, err := r.state.binding(space, sharedOwner, spec.Name)
		if err != nil {
			return nil, nil, err
		}
		if len(binding) != 0 {
			return nil, nil, fmt.Errorf("remove service %s before changing its provisioner", spec.Name)
		}
		state = &addons.Provisioning{Type: spec.Type, Instance: &addonv1.Instance{
			Installation: dockerInstallation, Space: space, Name: spec.Name,
			Runtime: "docker", Namespace: netName(space),
		}}
	}
	params, err := addons.ResolveInstanceParameters(desc, spec.Parameters, state.Parameters)
	if err != nil {
		return nil, nil, rt.UserError{Err: err}
	}
	state.Parameters, state.Protocol, state.Error = params, addons.NormalizeProtocol(desc.Protocol), ""
	if err := r.savePluginState(space, state); err != nil {
		return nil, nil, err
	}
	info, err := plugin.Provision(ctx, &addonv1.ProvisionRequest{Instance: state.Instance, Parameters: params.Struct()}, nil)
	if err != nil {
		state.Error = failureMessage(err)
		if saveErr := r.savePluginState(space, state); saveErr != nil {
			log.Printf("service %s/%s: recording provisioning failure: %v", space, spec.Name, saveErr)
		}
		return nil, nil, err
	}
	if effective := addons.ParametersFrom(info.Parameters); len(effective) > 0 {
		merged := map[string]any{}
		for k, v := range params {
			merged[k] = v
		}
		for k, v := range effective {
			merged[k] = v
		}
		if params, err = addons.ResolveParameters(desc.Parameters, merged); err != nil {
			return nil, nil, fmt.Errorf("addon plugin %s reported effective parameters its schema rejects: %w", plugin.Type(), err)
		}
	}
	state.Info, state.Parameters, state.Stopped, state.Error = info, params, false, ""
	if err := r.savePluginState(space, state); err != nil {
		return nil, nil, err
	}
	if err := r.state.setBinding(space, sharedOwner, spec.Name, info.Env); err != nil {
		return nil, nil, err
	}
	metadata := addons.Metadata{Type: plugin.Type(), Protocol: state.Protocol, Parameters: params}
	if err := r.state.writeJSON(metadata, "service-metadata", space, spec.Name+".json"); err != nil {
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

// deprovisionPlugin asks the plugin to remove (or stop) the instance. With
// force, a plugin that is gone or failing no longer blocks: the record is
// dropped and the backing service left to the operator.
func (r *Runtime) deprovisionPlugin(ctx context.Context, space, name string, keepData, force bool) error {
	state, err := r.pluginState(space, name)
	if err != nil || state == nil {
		return err
	}
	plugin, err := state.Plugin()
	if err == nil {
		err = plugin.Deprovision(ctx, state.Instance, keepData)
	}
	if err != nil {
		if !force {
			return fmt.Errorf("%w — remove with --orphan to drop donkey's record and leave the backing service behind", err)
		}
		log.Printf("service %s/%s: orphaning plugin resources: %v", space, name, err)
	}
	if keepData {
		state.Stopped = true
		if err := r.savePluginState(space, state); err != nil {
			return err
		}
		return r.state.removeBinding(space, sharedOwner, name)
	}
	return nil
}

func (r *Runtime) pluginServiceNames(space string) ([]string, error) {
	entries, err := os.ReadDir(r.state.path("provisioning", space))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(entry.Name(), ".json"))
	}
	return names, nil
}

// unboundPluginServices lists plugin instances that have no binding right
// now — pending, failed or stopped — so the console can show and remove them.
func (r *Runtime) unboundPluginServices(space string, bound map[string]bool) ([]rt.ServiceBindingInfo, error) {
	names, err := r.pluginServiceNames(space)
	if err != nil {
		return nil, err
	}
	var out []rt.ServiceBindingInfo
	for _, name := range names {
		if bound[name] {
			continue
		}
		state, err := r.pluginState(space, name)
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

func (r *Runtime) deprovisionSpacePlugins(ctx context.Context, space string) error {
	names, err := r.pluginServiceNames(space)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := r.deprovisionPlugin(ctx, space, name, false, false); err != nil {
			return err
		}
	}
	return nil
}
