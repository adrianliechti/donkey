package server

import (
	"context"
	"fmt"
	"reflect"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

func (s *Server) validateSpaceOfferings(ctx context.Context, settings *config.SpaceConfig, previous config.SpaceConfig) error {
	previous = previous.NormalizeOfferings()
	for id, entry := range settings.Offerings {
		// Unrelated settings edits must work during a provider outage. An
		// unchanged entry also survives removal of its provider registration.
		if old, ok := previous.Offerings[id]; ok && reflect.DeepEqual(old, entry) {
			continue
		}
		a, err := addons.Get(ctx, id)
		if err != nil {
			return fmt.Errorf("offerings.%s: %w", id, err)
		}
		desc, err := a.Describe(ctx)
		if err != nil {
			return fmt.Errorf("offerings.%s: %w", id, err)
		}
		entry.Defaults, err = addons.ResolveDefaults(desc.Parameters, entry.Defaults)
		if err != nil {
			return fmt.Errorf("offerings.%s.defaults: %w", id, err)
		}
		entry.Config, err = addons.ResolveDefaults(desc.SpaceConfiguration, entry.Config)
		if err != nil {
			return fmt.Errorf("offerings.%s.config: %w", id, err)
		}
		settings.Offerings[id] = entry
	}
	return nil
}

func (s *Server) offeringAvailable(ctx context.Context, space, id string) bool {
	a, err := addons.Get(ctx, id)
	if err != nil {
		return false
	}
	desc, err := a.Describe(ctx)
	if err != nil || desc.Deprecated {
		return false
	}
	_, err = runtime.PrepareService(ctx, s.rt, s.cfg.CommonOfferings, space, desc, nil, false)
	return err == nil
}
