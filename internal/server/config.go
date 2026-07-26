// Package server is donkey's control plane: a REST API running inside the
// cluster, building with the in-cluster BuildKit and applying the same
// manifests the CLI used to apply directly. Identity comes from the donkey
// gateway in front of it (OIDC login, forwarded as headers) or from a
// sealed CLI token it verifies itself.
package server

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/adrianliechti/donkey/internal/config"
)

// Version is stamped via -ldflags "-X github.com/adrianliechti/donkey/internal/server.Version=…".
// It rides on /v1/ping so a CLI can tell when it is talking to an older or
// newer control plane than itself.
var Version = "dev"

type Config struct {
	Listen string

	Registry        config.Registry
	Domain          string
	BuildkitAddr    string
	NamespacePrefix string

	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string

	// DefaultPlatform is the build platform when neither the deploy nor
	// donkey.yaml names one — matches the cluster's nodes.
	DefaultPlatform string

	// SessionKey is the installation's sealing key — the server verifies
	// the CLI tokens the gateway mints with it (the direct, port-forwarded
	// path next to the gateway's identity headers).
	SessionKey []byte
	// Admins narrows who may administer (users/groups/roles). Empty means
	// everyone the IdP admits.
	Admins  *config.AccessConfig
	AIModel string
}

func FromEnv() (*Config, error) {
	c := &Config{
		Listen: envOr("DONKEY_LISTEN", ":8088"),
		Registry: config.Registry{
			URL:       os.Getenv("DONKEY_REGISTRY_URL"),
			Username:  os.Getenv("DONKEY_REGISTRY_USERNAME"),
			Insecure:  os.Getenv("DONKEY_REGISTRY_INSECURE") == "true",
			Immutable: os.Getenv("DONKEY_REGISTRY_IMMUTABLE") == "true",
			CAFile:    os.Getenv("DONKEY_REGISTRY_CA_FILE"),
		},
		Domain:           os.Getenv("DONKEY_DOMAIN"),
		BuildkitAddr:     os.Getenv("DONKEY_BUILDKIT_ADDR"),
		NamespacePrefix:  envOr("DONKEY_NAMESPACE_PREFIX", config.DefaultNamespacePrefix),
		OIDCIssuer:       os.Getenv("DONKEY_OIDC_ISSUER"),
		OIDCClientID:     os.Getenv("DONKEY_OIDC_CLIENT_ID"),
		OIDCClientSecret: os.Getenv("DONKEY_OIDC_CLIENT_SECRET"),
		DefaultPlatform:  envOr("DONKEY_DEFAULT_PLATFORM", "linux/amd64"),
		AIModel:          envOr("DONKEY_AI_MODEL", config.DefaultAIModel),
	}
	key, err := hex.DecodeString(os.Getenv("DONKEY_SESSION_KEY"))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("DONKEY_SESSION_KEY must be 32 bytes of hex — `donkey server install` generates it")
	}
	c.SessionKey = key
	if raw := os.Getenv("DONKEY_ADMINS"); raw != "" {
		var admins config.AccessConfig
		if err := json.Unmarshal([]byte(raw), &admins); err != nil {
			return nil, fmt.Errorf("DONKEY_ADMINS is not valid AccessConfig JSON: %w", err)
		}
		c.Admins = &admins
	}
	if c.Registry.URL == "" {
		return nil, fmt.Errorf("DONKEY_REGISTRY_URL is required")
	}
	if err := c.Registry.Validate(); err != nil {
		return nil, err
	}
	if c.Domain == "" {
		return nil, fmt.Errorf("DONKEY_DOMAIN is required")
	}
	if c.BuildkitAddr == "" {
		return nil, fmt.Errorf("DONKEY_BUILDKIT_ADDR is required")
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

// AppHost is the address of an app: <app>.<space>.<domain>. The space in
// the middle is what lets two teams both have a "web". An internal app has no
// address at all — its siblings reach it by service name.
func (c *Config) AppHost(space string, app *config.AppConfig) string {
	if app.Internal {
		return ""
	}
	if app.Domain != "" {
		return app.Domain
	}
	return app.Name + "." + space + "." + c.Domain
}

func (c *Config) AppURL(space string, app *config.AppConfig) string {
	host := c.AppHost(space, app)
	if host == "" {
		return ""
	}
	return c.Scheme() + "://" + host
}
