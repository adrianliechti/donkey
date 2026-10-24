package docker

import (
	"context"
	"fmt"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
)

func (r *Runtime) PlatformConfig(context.Context) (config.PlatformConfig, error) {
	var settings config.PlatformConfig
	found, err := r.state.readRecord(&settings, "platform", "config")
	if err != nil {
		return settings, fmt.Errorf("cannot read platform settings: %w", err)
	}
	if !found {
		return settings, rt.ErrPlatformNotFound
	}
	if settings.Admins.Empty() {
		return settings, fmt.Errorf("platform settings must contain an admin")
	}
	return settings, settings.Validate()
}

func (r *Runtime) InitPlatformConfig(ctx context.Context, initial config.PlatformConfig) (config.PlatformConfig, error) {
	if err := initial.Validate(); err != nil {
		return config.PlatformConfig{}, err
	}
	if initial.Admins.Empty() {
		return config.PlatformConfig{}, fmt.Errorf("platform settings must contain an admin")
	}
	if err := r.state.storeRecord(initial, false, "platform", "config"); err != nil {
		return config.PlatformConfig{}, err
	}
	return r.PlatformConfig(ctx)
}

func (r *Runtime) SetPlatformConfig(ctx context.Context, settings config.PlatformConfig) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	if settings.Admins.Empty() {
		return fmt.Errorf("platform settings must contain an admin")
	}
	if _, err := r.PlatformConfig(ctx); err != nil {
		return err
	}
	return r.state.writeRecord(settings, "platform", "config")
}
