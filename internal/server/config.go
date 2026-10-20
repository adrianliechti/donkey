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

	Registry config.Registry
	// RegistryMirrors lets the solve-session token client apply Registry's
	// private CA to the exact mirror hosts configured in buildkitd.
	RegistryMirrors map[string]string
	Domain          string
	BuildkitAddr    string
	PackagesURL     string
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
	// only their own space, donkey-ai, donkey-proxy, donkey-telemetry and DNS, never the
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
	// OIDCScope is donkey's own API (e.g. api://<donkey-client-id>/<scope>):
	// the user's token console viewers hand on to the services they call.
	OIDCScope string

	// DefaultPlatform is the build platform when neither the deploy nor
	// donkey.yaml names one — matches the cluster's nodes.
	DefaultPlatform string

	// Build is the builder's settings and the environment every build gets.
	Build config.BuildSettings
	// AddonImages are repositories pulled directly by the runtime for managed
	// services; BuildKit's registry mirrors do not participate in those pulls.
	AddonImages config.InstallAddonImages
	// BusyboxImage is pulled by the runtime for file and volume helpers.
	BusyboxImage string
	// AddonImagePullSecrets names existing secrets in each workload namespace
	// that Kubernetes should use when pulling managed service and file-helper images.
	AddonImagePullSecrets []string
	AddonPlugins          config.AddonPlugins
	CommonOfferings       config.CommonOfferings
	ProxyServices         config.ProxyServices
	// Runtime names the provider in use (kubernetes or docker); the catalog
	// hides services a plugin does not offer there.
	Runtime string
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
	// PlatformAdmins optionally seeds the persisted platform settings. With no
	// seed, the first verified sign-in becomes the first platform admin.
	PlatformAdmins *config.AccessConfig
	AIModel        string
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
		OIDCScope:             os.Getenv("DONKEY_OIDC_SCOPE"),
		DefaultPlatform:       envOr("DONKEY_DEFAULT_PLATFORM", "linux/amd64"),
		BusyboxImage:          envOr("DONKEY_BUSYBOX_IMAGE", config.DefaultBusyboxImage),
		AddonImages: config.InstallAddonImages{
			Postgres: os.Getenv("DONKEY_POSTGRES_IMAGE"),
			Redis:    os.Getenv("DONKEY_REDIS_IMAGE"),
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
	if raw := os.Getenv("DONKEY_REGISTRY_MIRRORS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &c.RegistryMirrors); err != nil {
			return nil, fmt.Errorf("DONKEY_REGISTRY_MIRRORS is not a JSON object of registry-to-mirror mappings: %w", err)
		}
	}
	if raw := os.Getenv("DONKEY_ADDON_PLUGINS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &c.AddonPlugins); err != nil {
			return nil, fmt.Errorf("DONKEY_ADDON_PLUGINS must be a JSON array of addon plugin configurations: %w", err)
		}
	}
	if err := c.AddonPlugins.Validate(); err != nil {
		return nil, err
	}
	if raw := os.Getenv("DONKEY_COMMON_OFFERINGS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &c.CommonOfferings); err != nil {
			return nil, fmt.Errorf("DONKEY_COMMON_OFFERINGS must be a JSON array of offering ids: %w", err)
		}
	}
	if err := c.CommonOfferings.Validate(); err != nil {
		return nil, err
	}
	proxyServices := []byte(os.Getenv("DONKEY_PROXY_SERVICES"))
	if file := os.Getenv("DONKEY_PROXY_SERVICES_FILE"); file != "" {
		var err error
		proxyServices, err = os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("reading proxy service configuration: %w", err)
		}
	}
	if len(proxyServices) > 0 {
		if err := json.Unmarshal(proxyServices, &c.ProxyServices); err != nil {
			return nil, fmt.Errorf("proxy service configuration must be a JSON array")
		}
	}
	if err := c.ProxyServices.Validate(); err != nil {
		return nil, err
	}
	c.Runtime = os.Getenv("DONKEY_RUNTIME")
	if c.Runtime == "" {
		c.Runtime = "kubernetes"
	}
	// The build environment is a map, so it travels as JSON — the same way
	// DONKEY_PLATFORM_ADMINS does.
	if raw := os.Getenv("DONKEY_BUILD_ENV"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &c.Build.Env); err != nil {
			return nil, fmt.Errorf("DONKEY_BUILD_ENV is not a JSON object of build variables: %w", err)
		}
	}
	c.Build.VerifySeconds = envInt("DONKEY_BUILDER_VERIFY_SECONDS")
	c.Build.Attempts = envInt("DONKEY_BUILDER_ATTEMPTS")
	c.Build.Instructions = os.Getenv("DONKEY_BUILDER_INSTRUCTIONS")
	c.Build.DebianMirror = os.Getenv("DONKEY_BUILDER_DEBIAN_MIRROR") == "true"
	c.Build.AlpineMirror = os.Getenv("DONKEY_BUILDER_ALPINE_MIRROR") == "true"
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
	if raw := os.Getenv("DONKEY_PLATFORM_ADMINS"); raw != "" {
		var admins config.AccessConfig
		if err := json.Unmarshal([]byte(raw), &admins); err != nil {
			return nil, fmt.Errorf("DONKEY_PLATFORM_ADMINS is not valid AccessConfig JSON: %w", err)
		}
		c.PlatformAdmins = &admins
	}
	if c.PlatformAdmins != nil {
		if err := (config.PlatformConfig{Admins: *c.PlatformAdmins}).Validate(); err != nil {
			return nil, err
		}
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
	if err := config.ValidateBusyboxImage(c.BusyboxImage); err != nil {
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
	if err := (config.InstallBuild{BuildSettings: c.Build, RegistryMirrors: c.RegistryMirrors}).Validate(); err != nil {
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
