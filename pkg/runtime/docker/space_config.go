package docker

import (
	"context"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/network"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
)

func (r *Runtime) EnsureSpaceConfigs(ctx context.Context) error {
	spaces, err := r.ListSpaces(ctx)
	if err != nil {
		return err
	}
	for _, space := range spaces {
		if err := r.ValidateSpace(space); err != nil {
			return err
		}
		// Only missing records are created; configured spaces are never reset.
		if err := r.state.storeJSON(config.SpaceConfig{Domains: []string{}}, false, "spaces", space, "config.json"); err != nil {
			return err
		}
		if err := r.state.migrateSharedBindings(space); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) SpaceConfig(ctx context.Context, space string) (config.SpaceConfig, error) {
	var access config.SpaceConfig
	if err := r.ValidateSpace(space); err != nil {
		return access, err
	}
	nw, err := r.cli.NetworkInspect(ctx, netName(space), network.InspectOptions{})
	if errdefs.IsNotFound(err) {
		return access, rt.ErrSpaceNotFound
	}
	if err != nil {
		return access, err
	}
	if !sameIdentity(nw.Labels, resourceLabels(space, roleSpace)) {
		return access, fmt.Errorf("the network is not this installation's space")
	}
	if _, err := r.state.readJSON(&access, "spaces", space, "config.json"); err != nil {
		return access, err
	}
	if err := access.Validate(); err != nil {
		return access, err
	}
	return access, nil
}

func (r *Runtime) SetSpaceConfig(ctx context.Context, space string, access config.SpaceConfig) error {
	if access.Domains == nil {
		access.Domains = []string{}
	}
	if err := access.Validate(); err != nil {
		return err
	}
	_, err := r.SpaceConfig(ctx, space)
	if err != nil {
		return err
	}
	return r.state.writeJSON(access, "spaces", space, "config.json")
}
