package config

import (
	"fmt"
	"maps"
	"os"

	"sigs.k8s.io/yaml"
)

// InstallFile is the values file for `donkey server install -f server.yaml`:
// everything an admin configures about a donkey installation. Flags override
// individual fields; secrets can come from env instead
// (DONKEY_REGISTRY_PASSWORD, DONKEY_OIDC_CLIENT_SECRET, OPENAI_API_KEY).
type InstallFile struct {
	// Runtime is where donkey runs apps: "kubernetes" (the default) or
	// "docker" — straight on a Docker engine, no cluster.
	Runtime string `json:"runtime,omitempty"`
	// Image is the donkey-server image, pullable by the cluster.
	Image string `json:"image,omitempty"`
	// BuildkitImage is the build engine's image (default
	// DefaultBuildkitImage) — an air-gapped installation mirrors it.
	BuildkitImage string          `json:"buildkitImage,omitempty"`
	Registry      InstallRegistry `json:"registry,omitempty"`
	// Domain: apps become <app>.<space>.<domain>.
	Domain string `json:"domain,omitempty"`
	// ServerHost is where the API and console live (default: the domain
	// itself). It must be the domain or a name under it — the central OAuth
	// callback always stays on the domain, and the login cookie's domain has
	// to cover the console too.
	ServerHost string         `json:"serverHost,omitempty"`
	Gateway    InstallGateway `json:"gateway,omitempty"`
	TLS        InstallTLS     `json:"tls,omitempty"`
	// DefaultPlatform is the build platform of the cluster's nodes
	// (default linux/amd64).
	DefaultPlatform string `json:"defaultPlatform,omitempty"`

	Build InstallBuild `json:"build,omitempty"`

	Auth InstallAuth `json:"auth,omitempty"`
	AI   InstallAI   `json:"ai,omitempty"`
}

// InstallBuild is how apps are built and how a build reaches the outside
// world. BuildSettings travels to donkey-server as environment; the fields
// below are consumed where the installation is written — mirrors into the
// build engine's config, the proxy into both pods.
type InstallBuild struct {
	BuildSettings `json:",inline"`

	// RegistryMirrors maps an upstream registry host to its mirror. Written
	// into the build engine's config, so it covers every FROM a build pulls.
	RegistryMirrors map[string]string `json:"registryMirrors,omitempty"`

	// Proxy is a forward proxy for donkey-server and the build engine
	// themselves — not for the app's build steps, which read Env.
	Proxy InstallProxy `json:"proxy,omitempty"`
}

// BuildSettings is the build configuration donkey-server acts on. There is
// one builder, so its settings sit directly under build:.
type BuildSettings struct {
	// Env is injected into every build, under the app's own build.env.
	// BuildKit hands it to every RUN, which is what puts a package manager's
	// mirror in reach of build steps donkey never sees.
	Env map[string]string `json:"env,omitempty"`

	// Verify is a pointer so unset ≠ off; unset means on.
	Verify        *bool `json:"verify,omitempty"`
	VerifySeconds int   `json:"verifySeconds,omitempty"`
	// Attempts is the first Dockerfile plus one revision per failure shown.
	Attempts int `json:"attempts,omitempty"`
}

// InstallProxy is the forward proxy donkey's own processes use.
type InstallProxy struct {
	HTTP    string `json:"http,omitempty"`
	HTTPS   string `json:"https,omitempty"`
	NoProxy string `json:"noProxy,omitempty"`
}

const (
	// Long enough for a JVM or a cold interpreter, short enough that a broken
	// image fails quickly.
	DefaultVerifySeconds = 45
	// The first Dockerfile plus two revisions. Beyond that a model that has
	// seen the failure twice is not converging.
	DefaultAttempts = 3
)

// Validate checks what can be wrong without consulting the runtime.
func (b InstallBuild) Validate() error {
	if b.Attempts < 0 {
		return fmt.Errorf("build.attempts cannot be negative")
	}
	if b.VerifySeconds < 0 {
		return fmt.Errorf("build.verifySeconds cannot be negative")
	}
	for host, mirror := range b.RegistryMirrors {
		if host == "" {
			return fmt.Errorf("build.registryMirrors has an entry with no registry host")
		}
		if mirror == "" {
			return fmt.Errorf("build.registryMirrors[%q] has no mirror address", host)
		}
	}
	return nil
}

// VerifyStartup is on unless an installation turns it off.
func (b BuildSettings) VerifyStartup() bool {
	return b.Verify == nil || *b.Verify
}

// VerifyWindow is how long the app is given to come up.
func (b BuildSettings) VerifyWindow() int {
	if b.VerifySeconds <= 0 {
		return DefaultVerifySeconds
	}
	return b.VerifySeconds
}

// BuildAttempts is how many Dockerfiles the assistant may write.
func (b BuildSettings) BuildAttempts() int {
	if b.Attempts <= 0 {
		return DefaultAttempts
	}
	return b.Attempts
}

// MergeEnv layers an app's build.env over the installation's; the app wins.
func (b BuildSettings) MergeEnv(appEnv map[string]string) map[string]string {
	if len(b.Env) == 0 {
		return appEnv
	}
	merged := make(map[string]string, len(b.Env)+len(appEnv))
	maps.Copy(merged, b.Env)
	maps.Copy(merged, appEnv)
	return merged
}

// InstallRegistry describes the registry app images are pushed to. CASecret
// covers the common in-house case: a registry whose certificate comes from
// the cluster's own CA, which the server and the build engine must trust.
type InstallRegistry struct {
	Registry
	// CASecret is a secret in donkey-system holding ca.crt.
	CASecret string `json:"caSecret,omitempty"`
}

// InstallAuth configures the login gateway — the one way in: apps, console
// and CLI all authenticate against this issuer.
type InstallAuth struct {
	Issuer       string `json:"issuer,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
	// Admins narrows who may use the console and CLI (users/groups/roles,
	// matched like an app's `access` rules). Empty means everyone the IdP
	// admits may administer.
	Admins *AccessConfig `json:"admins,omitempty"`
}

// InstallGateway tunes the traffic entry point.
type InstallGateway struct {
	// ServiceType is the type of the service traffic enters the cluster
	// through: LoadBalancer (the default), NodePort or ClusterIP. Clusters
	// without a load-balancer controller (kind, local demos) need NodePort
	// or ClusterIP — a LoadBalancer service never gets an address there,
	// which would leave every donkey hostname unroutable.
	ServiceType string `json:"serviceType,omitempty"`
	// HTTPNodePort/HTTPSNodePort pin the node ports of a NodePort service,
	// so port mappings (kind) or firewall rules can rely on them.
	HTTPNodePort  int32 `json:"httpNodePort,omitempty"`
	HTTPSNodePort int32 `json:"httpsNodePort,omitempty"`
}

// InstallTLS is how the gateway gets its certificates. donkey always serves
// apps over https — the login gateway's cookies require it — and the
// gateway issues the certificates itself: no cert-manager, no ingress
// vendor.
type InstallTLS struct {
	// Mode is "acme" (one certificate per hostname from Let's Encrypt, the
	// default — the gateway must be reachable on ports 80/443), "ca" (a
	// self-signed CA minting per-hostname certificates — local and internal
	// installs; trust its one CA certificate and every app is covered), or
	// "static" (a provided certificate, usually a wildcard, in the Secret
	// named by Secret).
	Mode string `json:"mode,omitempty"`
	// Email is the ACME account contact (expiry warnings); optional.
	Email string `json:"email,omitempty"`
	// Directory overrides the ACME endpoint — Let's Encrypt staging, or an
	// internal CA speaking ACME.
	Directory string `json:"directory,omitempty"`
	// Secret names a kubernetes.io/tls Secret in donkey-system holding the
	// certificate for static mode (default donkey-gateway-tls).
	Secret string `json:"secret,omitempty"`

	// Issuer and Kind configured cert-manager in earlier donkey versions.
	// They are ignored now — kept so old install files still parse.
	Issuer string `json:"issuer,omitempty"`
	Kind   string `json:"kind,omitempty"`
}

// DefaultBuildkitImage is the build engine donkey installs. It is pinned to a
// digest as well as a tag: a tag can be re-pushed, and the build engine runs
// privileged — the one place where "whatever the registry serves today" is
// not good enough. The digest is the multi-platform index of v0.31.2.
const DefaultBuildkitImage = "moby/buildkit:v0.31.2@sha256:2f5adac4ecd194d9f8c10b7b5d7bceb5186853db1b26e5abd3a657af0b7e26ec"

type InstallAI struct {
	BaseURL string `json:"baseUrl,omitempty"`
	APIKey  string `json:"apiKey,omitempty"`
	Model   string `json:"model,omitempty"`
}

func LoadInstallFile(path string) (*InstallFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("couldn't read the settings file: %w", err)
	}
	var f InstallFile
	if err := yaml.UnmarshalStrict(b, &f); err != nil {
		return nil, fmt.Errorf("%s is not valid: %w", path, err)
	}
	return &f, nil
}
