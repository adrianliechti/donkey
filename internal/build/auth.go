package build

import (
	"context"

	clitypes "github.com/docker/cli/cli/config/types"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"

	"github.com/adrianliechti/donkey/internal/config"
)

// registryAuth builds an in-process BuildKit auth provider from donkey's own
// registry settings — no `docker login`, no ~/.docker/config.json.
func registryAuth(reg config.Registry) (session.Attachable, error) {
	return authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{
		AuthConfigProvider: func(ctx context.Context, h string, scope []string, _ authprovider.ExpireCachedAuthCheck) (clitypes.AuthConfig, error) {
			return registryAuthConfig(reg, h)
		},
	}), nil
}

func registryAuthConfig(reg config.Registry, host string) (clitypes.AuthConfig, error) {
	if sameRegistryHost(host, reg.Host()) {
		username, password, err := reg.Credentials()
		if err != nil {
			return clitypes.AuthConfig{}, err
		}
		if username != "" && password != "" {
			return clitypes.AuthConfig{Username: username, Password: password, ServerAddress: host}, nil
		}
	}
	if reg.DockerConfigDir != "" {
		username, password, found, err := config.DockerConfigDirCredentials(reg.DockerConfigDir, host)
		if err != nil {
			return clitypes.AuthConfig{}, err
		}
		if found {
			return clitypes.AuthConfig{Username: username, Password: password, ServerAddress: host}, nil
		}
	}
	return clitypes.AuthConfig{}, nil
}

func sameRegistryHost(a, b string) bool {
	return a == b || (b == "docker.io" && (a == "registry-1.docker.io" || a == "https://index.docker.io/v1/"))
}
