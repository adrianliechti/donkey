package build

import (
	"context"

	clitypes "github.com/docker/cli/cli/config/types"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"

	"monobox.ch/donkey/internal/config"
)

// registryAuth builds an in-process BuildKit auth provider from donkey's own
// registry settings — no `docker login`, no ~/.docker/config.json.
func registryAuth(reg config.Registry) (session.Attachable, error) {
	password, err := reg.Password()
	if err != nil {
		return nil, err
	}
	host := reg.Host()
	return authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{
		AuthConfigProvider: func(ctx context.Context, h string, scope []string, _ authprovider.ExpireCachedAuthCheck) (clitypes.AuthConfig, error) {
			if reg.Username == "" || password == "" {
				return clitypes.AuthConfig{}, nil
			}
			// Docker Hub asks with its canonical hosts.
			if h == host || (host == "docker.io" && (h == "registry-1.docker.io" || h == "https://index.docker.io/v1/")) {
				return clitypes.AuthConfig{Username: reg.Username, Password: password, ServerAddress: h}, nil
			}
			return clitypes.AuthConfig{}, nil
		},
	}), nil
}
