package docker

import (
	"context"
	"errors"
	"fmt"
	cerrdefs "github.com/containerd/errdefs"
	"io/fs"
	"os"
	"strings"

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

func (r *Runtime) provisionPlugin(ctx context.Context, space string, spec rt.ServiceSpec, plugin *addons.Plugin, previous *addons.Provisioning) (map[string]string, string, string, error) {
	if _, err := r.cli.ContainerInspect(ctx, ctrName(space, spec.Name)); err == nil {
		return nil, "", "", fmt.Errorf("the name %q is already taken by a local workload", spec.Name)
	} else if !cerrdefs.IsNotFound(err) {
		return nil, "", "", err
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
		binding, err := r.state.binding(space, sharedOwner, spec.Name)
		if err != nil {
			return nil, "", "", err
		}
		if len(binding) != 0 {
			return nil, "", "", fmt.Errorf("remove service %s before changing its provisioner", spec.Name)
		}
		state = &addons.Provisioning{Type: spec.Type, Instance: &addonv1.Instance{
			Installation: dockerInstallation, Space: space, Name: spec.Name,
			Runtime: "docker", Namespace: netName(space),
		}}
	}
	if err := r.state.writeJSON(state, "provisioning", space, spec.Name+".json"); err != nil {
		return nil, "", "", err
	}
	info, err := plugin.Provision(ctx, &addonv1.ProvisionRequest{Instance: state.Instance, Version: version, Storage: storage})
	if err != nil {
		return nil, "", "", err
	}
	state.Info, state.Stopped = info, false
	if err := r.state.writeJSON(state, "provisioning", space, spec.Name+".json"); err != nil {
		return nil, "", "", err
	}
	if err := r.state.setBinding(space, sharedOwner, spec.Name, info.Env); err != nil {
		return nil, "", "", err
	}
	if err := r.state.writeJSON(addons.Metadata{Type: plugin.Type(), Protocol: info.Protocol}, "service-metadata", space, spec.Name+".json"); err != nil {
		return nil, "", "", err
	}
	return info.Env, version, storage, nil
}

func (r *Runtime) deprovisionPlugin(ctx context.Context, space, name string, keepData bool) error {
	state, err := r.pluginState(space, name)
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
	if keepData {
		state.Stopped = true
		if err := r.state.writeJSON(state, "provisioning", space, name+".json"); err != nil {
			return err
		}
		return r.state.removeBinding(space, sharedOwner, name)
	}
	return nil
}

func (r *Runtime) deprovisionSpacePlugins(ctx context.Context, space string) error {
	entries, err := os.ReadDir(r.state.path("provisioning", space))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if err := r.deprovisionPlugin(ctx, space, strings.TrimSuffix(entry.Name(), ".json"), false); err != nil {
			return err
		}
	}
	return nil
}
