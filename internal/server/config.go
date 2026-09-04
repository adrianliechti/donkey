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
	"strconv"
	"strings"

	"github.com/adrianliechti/donkey/internal/config"
)

// Version is stamped via -ldflags "-X github.com/adrianliechti/donkey/internal/server.Version=…".
// It rides on /v1/ping so a CLI can tell when it is talking to an older or
// newer control plane than itself.
var Version = "dev"

type Config struct {
	Listen string

	Registry     config.Registry
	Domain       string
	BuildkitAddr string
	PackagesURL  string
	// NamespacePrefix maps a space onto its own namespace; ignored when
	// WorkloadNamespace names one namespace for the whole installation.
	NamespacePrefix string
	// SystemNamespace is where the control plane lives — and the identity of
	// this installation, which is what keeps several donkeys on one cluster
	// out of each other's objects.
	SystemNamespace string
	// WorkloadNamespace, when set, is the single namespace every app lives
	// in; donkey neither creates nor deletes it.
	WorkloadNamespace string
	// IsolateSpaces fences every space with a NetworkPolicy (Kubernetes
	// runtime only — Docker spaces are separate networks and isolated as is).
	IsolateSpaces bool
	// RestrictEgress adds the egress half of that fence — a space's pods reach
	// only their own space, donkey-ai, donkey-telemetry and DNS, never the
	// internet. Off by default; only meaningful with IsolateSpaces.
	RestrictEgress bool
	// AIURL is donkey-ai's address — the console's /ai/v1 calls are
	// forwarded there. The address only, no path: what the console asks for
	// is what gets proxied.
	AIURL string
	// TelemetryURL is the private query listener of the traffic telemetry
	// service. App exporters use a different port and credential.
	TelemetryURL          string
	TelemetryOTLPEndpoint string

	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string

	// DefaultPlatform is the build platform when neither the deploy nor
	// donkey.yaml names one — matches the cluster's nodes.
	DefaultPlatform string

	// Build is the builder's settings and the environment every build gets.
	Build config.BuildSettings
	// AddonImages are repositories pulled directly by the runtime for managed
	// services; BuildKit's registry mirrors do not participate in those pulls.
	AddonImages config.InstallAddonImages
	// AddonImagePullSecrets names existing secrets in each workload namespace
	// that Kubernetes should use when pulling managed service images.
	AddonImagePullSecrets []string
	// TLS carries the cert-manager issuer used for exact-host Certificates in
	// Kubernetes. Docker's gateway issues certificates independently.
	TLS config.InstallTLS
	// ExternalDNS publishes optional exact-host DNSEndpoints at the gateway
	// VIP as apps are exposed.
	ExternalDNS config.InstallExternalDNS

	// SessionKey is the installation's sealing key — the server verifies
	// the CLI tokens the gateway mints with it (the direct, port-forwarded
	// path next to the gateway's identity headers).
	SessionKey []byte
	// TelemetryKey authenticates the gateway, app exporters, and private query
	// client independently of login-session rotation.
	TelemetryKey []byte
	// Admins narrows who may administer (users/groups/roles). Empty means
	// everyone the IdP admits.
	Admins  *config.AccessConfig
	AIModel string
}

func FromEnv() (*Config, error) {
	c := &Config{
		Listen: envOr("DONKEY_LISTEN", ":8088"),
		Registry: config.Registry{
			URL:             os.Getenv("DONKEY_REGISTRY_URL"),
			Username:        os.Getenv("DONKEY_REGISTRY_USERNAME"),
			DockerConfigDir: os.Getenv("DONKEY_REGISTRY_CONFIG_DIR"),
			Insecure:        os.Getenv("DONKEY_REGISTRY_INSECURE") == "true",
			Immutable:       os.Getenv("DONKEY_REGISTRY_IMMUTABLE") == "true",
			CAFile:          os.Getenv("DONKEY_REGISTRY_CA_FILE"),
		},
		Domain:            os.Getenv("DONKEY_DOMAIN"),
		BuildkitAddr:      os.Getenv("DONKEY_BUILDKIT_ADDR"),
		PackagesURL:       envOr("DONKEY_PACKAGES_URL", "http://donkey-packages"),
		NamespacePrefix:   envOr("DONKEY_NAMESPACE_PREFIX", config.DefaultNamespacePrefix),
		SystemNamespace:   envOr("DONKEY_SYSTEM_NAMESPACE", config.DefaultSystemNamespace),
		WorkloadNamespace: os.Getenv("DONKEY_WORKLOAD_NAMESPACE"),
		// Isolation is on by default, so only an explicit "false" opens the
		// network — including on manifests that predate the setting.
		IsolateSpaces: os.Getenv("DONKEY_SPACE_ISOLATION") != "false",
		// Egress restriction is opt-in — it can cut an app off from the
		// internet, so it is on only when the installation asked for it.
		RestrictEgress:        os.Getenv("DONKEY_RESTRICT_EGRESS") == "true",
		AIURL:                 os.Getenv("DONKEY_AI_URL"),
		TelemetryURL:          envOr("DONKEY_TELEMETRY_URL", "http://donkey-telemetry:9090"),
		TelemetryOTLPEndpoint: envOr("DONKEY_TELEMETRY_OTLP_ENDPOINT", "http://donkey-telemetry:4318"),
		OIDCIssuer:            os.Getenv("DONKEY_OIDC_ISSUER"),
		OIDCClientID:          os.Getenv("DONKEY_OIDC_CLIENT_ID"),
		OIDCClientSecret:      os.Getenv("DONKEY_OIDC_CLIENT_SECRET"),
		DefaultPlatform:       envOr("DONKEY_DEFAULT_PLATFORM", "linux/amd64"),
		AddonImages: config.InstallAddonImages{
			Postgres: os.Getenv("DONKEY_POSTGRES_IMAGE"),
			RustFS:   os.Getenv("DONKEY_RUSTFS_IMAGE"),
		},
		AddonImagePullSecrets: splitCommaSeparated(os.Getenv("DONKEY_ADDON_IMAGE_PULL_SECRETS")),
		TLS: config.InstallTLS{
			Mode:        os.Getenv("DONKEY_TLS_MODE"),
			IssuerName:  os.Getenv("DONKEY_TLS_ISSUER_NAME"),
			IssuerKind:  os.Getenv("DONKEY_TLS_ISSUER_KIND"),
			IssuerGroup: os.Getenv("DONKEY_TLS_ISSUER_GROUP"),
		},
		ExternalDNS: config.InstallExternalDNS{
			Targets:    splitCommaSeparated(os.Getenv("DONKEY_EXTERNAL_DNS_TARGETS")),
			RecordType: os.Getenv("DONKEY_EXTERNAL_DNS_RECORD_TYPE"),
			TTL:        int64(envInt("DONKEY_EXTERNAL_DNS_TTL")),
		},
		AIModel: envOr("DONKEY_AI_MODEL", config.DefaultAIModel),
	}
	// The build environment is a map, so it travels as JSON — the same way
	// DONKEY_ADMINS does.
	if raw := os.Getenv("DONKEY_BUILD_ENV"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &c.Build.Env); err != nil {
			return nil, fmt.Errorf("DONKEY_BUILD_ENV is not a JSON object of build variables: %w", err)
		}
	}
	c.Build.VerifySeconds = envInt("DONKEY_BUILDER_VERIFY_SECONDS")
	c.Build.Attempts = envInt("DONKEY_BUILDER_ATTEMPTS")
	c.Build.Instructions = os.Getenv("DONKEY_BUILDER_INSTRUCTIONS")
	if v := os.Getenv("DONKEY_BUILDER_VERIFY"); v != "" {
		c.Build.Verify = new(v == "true")
	}
	c.Build.Provenance = os.Getenv("DONKEY_BUILDER_PROVENANCE")
	if err := c.Build.ValidateProvenance(); err != nil {
		return nil, err
	}
	c.Build.SBOM = os.Getenv("DONKEY_BUILDER_SBOM") == "true"
	key, err := hex.DecodeString(os.Getenv("DONKEY_SESSION_KEY"))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("DONKEY_SESSION_KEY must be 32 bytes of hex — `donkey server install` generates it")
	}
	c.SessionKey = key
	telemetryKey, err := hex.DecodeString(os.Getenv("DONKEY_TELEMETRY_KEY"))
	if err != nil || len(telemetryKey) != 32 {
		return nil, fmt.Errorf("DONKEY_TELEMETRY_KEY must be 32 bytes of hex — `donkey server install` generates it")
	}
	c.TelemetryKey = telemetryKey
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
	if err := c.AddonImages.Validate(); err != nil {
		return nil, err
	}
	if err := config.ValidateAddonImagePullSecrets(c.AddonImagePullSecrets); err != nil {
		return nil, err
	}
	if err := c.TLS.Validate(); err != nil {
		return nil, err
	}
	if err := c.ExternalDNS.Validate(); err != nil {
		return nil, err
	}
	if err := (config.InstallBuild{BuildSettings: c.Build}).Validate(); err != nil {
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

// envInt reads an optional numeric setting; anything unreadable falls back to
// the default the accessor applies.
func envInt(key string) int {
	n, err := strconv.Atoi(os.Getenv(key))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func splitCommaSeparated(raw string) []string {
	var values []string
	seen := make(map[string]bool)
	for value := range strings.SplitSeq(raw, ",") {
		if value = strings.TrimSpace(value); value != "" && !seen[value] {
			values = append(values, value)
			seen[value] = true
		}
	}
	return values
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Scheme is always https for apps: the login gateway's cookies require it.
func (c *Config) Scheme() string { return "https" }

// AppHost is the address of an app — see config.AppHost for the rule.
func (c *Config) AppHost(space string, app *config.AppConfig) string {
	return config.AppHost(app, space, c.Domain)
}

func (c *Config) AppURL(space string, app *config.AppConfig) string {
	host := c.AppHost(space, app)
	if host == "" {
		return ""
	}
	return c.Scheme() + "://" + host
}
