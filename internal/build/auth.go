package build

import (
	"context"
	"strings"

	clitypes "github.com/docker/cli/cli/config/types"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"

	"github.com/adrianliechti/donkey/internal/config"
)

// registryAuth builds an in-process BuildKit auth provider from donkey's own
// registry settings — no `docker login`, no ~/.docker/config.json.
func registryAuth(reg config.Registry, mirrors map[string]string) (session.Attachable, error) {
	return authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{
		AuthConfigProvider: func(ctx context.Context, h string, scope []string, _ authprovider.ExpireCachedAuthCheck) (clitypes.AuthConfig, error) {
			return registryAuthConfig(reg, h)
		},
		// buildkitd performs the registry request, but delegates Bearer token
		// requests back to this session provider. The daemon's buildkitd.toml CA
		// therefore is not enough: this client must trust it for the push
		// registry and every configured mirror host too.
		TLSConfigs: registryAuthTLSConfigs(reg, mirrors),
	}), nil
}

func registryAuthTLSConfigs(reg config.Registry, mirrors map[string]string) map[string]*authprovider.AuthTLSConfig {
	if reg.CAFile == "" {
		return nil
	}
	configs := map[string]*authprovider.AuthTLSConfig{}
	add := func(host string) {
		host = strings.ToLower(strings.TrimSpace(host))
		if host != "" {
			configs[host] = &authprovider.AuthTLSConfig{RootCAs: []string{reg.CAFile}}
		}
	}
	add(reg.Host())
	for _, mirror := range mirrors {
		if endpoint, err := config.ParseRegistryMirror(mirror); err == nil {
			add(endpoint.Host)
		}
	}
	return configs
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
