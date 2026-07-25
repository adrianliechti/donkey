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
	BuildkitAddr    string
	NamespacePrefix string

	// TLSIssuer/TLSIssuerKind name the cert-manager issuer that signs one
	// certificate per app hostname.
	TLSIssuer     string
	TLSIssuerKind string

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
			CAFile:   os.Getenv("DONKEY_REGISTRY_CA_FILE"),
		},
		BaseDomain:       os.Getenv("DONKEY_BASE_DOMAIN"),
		TLSIssuer:        os.Getenv("DONKEY_TLS_ISSUER"),
		TLSIssuerKind:    envOr("DONKEY_TLS_ISSUER_KIND", "ClusterIssuer"),
		BuildkitAddr:     os.Getenv("DONKEY_BUILDKIT_ADDR"),
		NamespacePrefix:  envOr("DONKEY_NAMESPACE_PREFIX", config.DefaultNamespacePrefix),
		GatewayName:      envOr("DONKEY_GATEWAY_NAME", "donkey"),
		GatewayNamespace: envOr("DONKEY_GATEWAY_NAMESPACE", "donkey-system"),
		OIDCIssuer:       os.Getenv("DONKEY_OIDC_ISSUER"),
		OIDCClientID:     os.Getenv("DONKEY_OIDC_CLIENT_ID"),
		OIDCClientSecret: os.Getenv("DONKEY_OIDC_CLIENT_SECRET"),
		DefaultPlatform:  envOr("DONKEY_DEFAULT_PLATFORM", "linux/amd64"),
		APIToken:         os.Getenv("DONKEY_API_TOKEN"),
		AIModel:          envOr("DONKEY_AI_MODEL", config.DefaultAIModel),
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
	// Apps are always served over https (the login gateway's cookies are
	// Secure-only), and their certificates come from cert-manager.
	if c.TLSIssuer == "" {
		return nil, fmt.Errorf("DONKEY_TLS_ISSUER is required — donkey needs a cert-manager issuer to give apps their certificates")
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

// Scheme is always https for apps: the login gateway's cookies require it.
func (c *Config) Scheme() string { return "https" }

// Namespace is where a space lives: all its apps share it.
func (c *Config) Namespace(space string) string { return c.NamespacePrefix + space }

// AppHost is the address of an app: <app>.<space>.<baseDomain>. The space in
// the middle is what lets two teams both have a "web".
func (c *Config) AppHost(space string, app *config.AppConfig) string {
	if app.Domain != "" {
		return app.Domain
	}
	return app.Name + "." + space + "." + c.BaseDomain
}

// SpaceWildcard is the certificate name that covers every app of a space.
func (c *Config) SpaceWildcard(space string) string { return "*." + space + "." + c.BaseDomain }

func (c *Config) AppURL(space string, app *config.AppConfig) string {
	return c.Scheme() + "://" + c.AppHost(space, app)
}
