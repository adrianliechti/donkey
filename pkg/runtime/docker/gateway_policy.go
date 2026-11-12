package docker

import (
	"errors"
	"io/fs"
	"os"

	"github.com/adrianliechti/donkey/internal/config"
)

// ReadGatewayPolicies reads only the shared state volume; the gateway does
// not need Docker daemon access to enforce current space membership.
func ReadGatewayPolicies(dir string) (map[string]config.SpaceConfig, config.PlatformConfig, error) {
	s := state{dir: dir}
	policies := map[string]config.SpaceConfig{}
	var platform config.PlatformConfig
	if _, err := s.readRecord(&platform, "platform", "config"); err != nil {
		return nil, platform, err
	}
	if err := platform.Validate(); err != nil {
		return nil, platform, err
	}
	spaces, err := os.ReadDir(s.path("spaces"))
	if errors.Is(err, fs.ErrNotExist) {
		return policies, platform, nil
	}
	if err != nil {
		return nil, platform, err
	}
	for _, space := range spaces {
		if !space.IsDir() || !config.ValidName(space.Name()) {
			continue
		}
		var policy config.SpaceConfig
		if _, err := s.readRecord(&policy, "spaces", space.Name(), "config"); err != nil {
			return nil, platform, err
		}
		if err := policy.Validate(); err != nil {
			return nil, platform, err
		}
		policies[space.Name()] = policy
	}
	return policies, platform, nil
}
