package runtime

import (
	"context"
	"errors"

	"github.com/adrianliechti/donkey/internal/config"
)

var ErrSpaceNotFound = errors.New("the space does not exist")
var ErrPlatformNotFound = errors.New("platform settings have not been initialized")

type spaceConfigReader interface {
	SpaceConfig(context.Context, string) (config.SpaceConfig, error)
}

func CheckAppDomain(ctx context.Context, provider spaceConfigReader, app *config.AppConfig, space, domain string) error {
	policy, err := provider.SpaceConfig(ctx, space)
	if errors.Is(err, ErrSpaceNotFound) {
		policy, err = config.DefaultSpaceConfig(space, domain), nil
	}
	if err != nil {
		return err
	}
	if err := policy.CheckAppHost(app, space, domain); err != nil {
		return UserError{Err: err}
	}
	return nil
}

// CheckSpaceDomains checks actual hostnames after the space has been created,
// including addresses inherited from shared-address routes.
func CheckSpaceDomains(ctx context.Context, provider spaceConfigReader, space string, hosts ...string) error {
	policy, err := provider.SpaceConfig(ctx, space)
	if err != nil {
		return err
	}
	for _, host := range hosts {
		if err := policy.CheckHost(host); err != nil {
			return UserError{Err: err}
		}
	}
	return nil
}
