// Package server is donkey's control plane: a token-authenticated REST API
// running inside the cluster, building with the in-cluster BuildKit and
// applying the same manifests the CLI used to apply directly.
package server

import (
	"fmt"
	"os"

	"monobox.ch/donkey/internal/config"
)

type Config struct {
	Listen string

	Registry        config.Registry
	BaseDomain      string
	HTTPS           bool
	BuildkitAddr    string
	NamespacePrefix string

	GatewayName      string
	GatewayNamespace string

	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string

	// DefaultPlatform is the build platform when neither the deploy nor
	// donkey.yaml names one — matches the cluster's nodes.
	DefaultPlatform string

	APIToken string
	AIModel  string
}

func FromEnv() (*Config, error) {
	c := &Config{
		Listen: envOr("DONKEY_LISTEN", ":8088"),
		Registry: config.Registry{
			URL:      os.Getenv("DONKEY_REGISTRY_URL"),
			Username: os.Getenv("DONKEY_REGISTRY_USERNAME"),
			Insecure: os.Getenv("DONKEY_REGISTRY_INSECURE") == "true",
		},
		BaseDomain:      os.Getenv("DONKEY_BASE_DOMAIN"),
		HTTPS:           os.Getenv("DONKEY_HTTPS") == "true",
		BuildkitAddr:    os.Getenv("DONKEY_BUILDKIT_ADDR"),
		NamespacePrefix: envOr("DONKEY_NAMESPACE_PREFIX", config.DefaultNamespacePrefix),
		GatewayName:      envOr("DONKEY_GATEWAY_NAME", "donkey"),
		GatewayNamespace: envOr("DONKEY_GATEWAY_NAMESPACE", "donkey-system"),
		OIDCIssuer:       os.Getenv("DONKEY_OIDC_ISSUER"),
		OIDCClientID:     os.Getenv("DONKEY_OIDC_CLIENT_ID"),
		OIDCClientSecret: os.Getenv("DONKEY_OIDC_CLIENT_SECRET"),
		DefaultPlatform:  envOr("DONKEY_DEFAULT_PLATFORM", "linux/amd64"),
		APIToken:        os.Getenv("DONKEY_API_TOKEN"),
		AIModel:         envOr("DONKEY_AI_MODEL", config.DefaultAIModel),
	}
	if c.Registry.URL == "" {
		return nil, fmt.Errorf("DONKEY_REGISTRY_URL is required")
	}
	if c.BaseDomain == "" {
		return nil, fmt.Errorf("DONKEY_BASE_DOMAIN is required")
	}
	if c.BuildkitAddr == "" {
		return nil, fmt.Errorf("DONKEY_BUILDKIT_ADDR is required")
	}
	if c.APIToken == "" {
		return nil, fmt.Errorf("DONKEY_API_TOKEN is required")
	}
	return c, nil
}

// The registry password flows through the same env var the Registry type
// already reads (DONKEY_REGISTRY_PASSWORD), set from the system secret.

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func (c *Config) Scheme() string {
	if c.HTTPS {
		return "https"
	}
	return "http"
}

func (c *Config) Namespace(app string) string { return c.NamespacePrefix + app }

func (c *Config) AppURL(app *config.AppConfig) string {
	domain := app.Domain
	if domain == "" {
		domain = app.Name + "." + c.BaseDomain
	}
	return c.Scheme() + "://" + domain
}
