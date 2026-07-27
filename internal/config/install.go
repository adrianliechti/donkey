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

// InstallBuild covers how apps are built: which builder runs for an app that
// brings no Dockerfile, and how every build reaches the outside world. An
// installation whose only egress is a mirror configures all of it here.
//
// It splits in two. BuildSettings is what donkey-server needs at build time
// and travels to it as environment; the fields below it are consumed where
// the installation is written — mirrors into the build engine's config file,
// the proxy into both pods' environment.
type InstallBuild struct {
	BuildSettings `json:",inline"`

	// RegistryMirrors maps an upstream registry host to the mirror serving
	// it ("docker.io": "https://jfrog.corp/docker-remote"). It is written
	// into the build engine's config, so it covers every image a build pulls
	// — the builder image, the run image and every FROM in a Dockerfile.
	RegistryMirrors map[string]string `json:"registryMirrors,omitempty"`

	// Proxy is a forward proxy for donkey-server and the build engine
	// themselves — not for the app's build steps, which read Env.
	Proxy InstallProxy `json:"proxy,omitempty"`
}

// BuildSettings is the part of the build configuration donkey-server itself
// acts on: which builder to run, and with what.
type BuildSettings struct {
	// Builder is the default for apps with no Dockerfile: "railpack" (the
	// default) or "ai". A donkey.yaml with an explicit build.type still
	// wins, and a Dockerfile always beats both.
	Builder BuildStrategy `json:"builder,omitempty"`

	// Env is injected into every app build, under the app's own build.env.
	// BuildKit hands these to every RUN, which is what puts a package
	// manager's mirror (NPM_CONFIG_REGISTRY, PIP_INDEX_URL, GOPROXY) in
	// reach of build steps donkey never sees.
	Env map[string]string `json:"env,omitempty"`

	AI       InstallAIBuilder `json:"ai,omitempty"`
	Railpack InstallRailpack  `json:"railpack,omitempty"`
}

// InstallProxy is the forward proxy donkey's own processes use, in the
// spelling every tool already understands.
type InstallProxy struct {
	HTTP    string `json:"http,omitempty"`
	HTTPS   string `json:"https,omitempty"`
	NoProxy string `json:"noProxy,omitempty"`
}

// InstallAIBuilder tunes donkey's own builder: the assistant writes the
// Dockerfile, donkey builds it and checks the result starts.
type InstallAIBuilder struct {
	// Verify runs the built image and waits for it to come up before the
	// image is published. A pointer so "unset" is distinguishable from
	// "off" — unset means on, because an image that cannot start is the one
	// failure a build is in a position to catch.
	Verify *bool `json:"verify,omitempty"`
	// VerifySeconds is how long the app is given to start listening
	// (default DefaultVerifySeconds).
	VerifySeconds int `json:"verifySeconds,omitempty"`
	// Attempts is how many Dockerfiles the assistant may write for one
	// deploy: the first, plus one revision per failure it is shown
	// (default DefaultAIAttempts).
	Attempts int `json:"attempts,omitempty"`
}

// InstallRailpack overrides the two images railpack's plans reference. They
// are ordinary registry references, so an installation behind a mirror points
// them at it.
type InstallRailpack struct {
	BuilderImage string `json:"builderImage,omitempty"`
	RuntimeImage string `json:"runtimeImage,omitempty"`
}

const (
	// DefaultVerifySeconds is how long a freshly built image is given to
	// start listening. Long enough for a JVM or a cold interpreter, short
	// enough that a genuinely broken image fails the deploy quickly.
	DefaultVerifySeconds = 45
	// DefaultAIAttempts is the first Dockerfile plus two revisions. Beyond
	// that a model that has seen the failure twice is not converging, and
	// the honest answer is to show the person the error.
	DefaultAIAttempts = 3
)

// Validate checks the parts of the build section that can be wrong on their
// own. The builder is the fallback for apps that bring no Dockerfile, so
// "dockerfile" and "auto" are not answers to it.
func (b InstallBuild) Validate() error {
	switch b.Builder {
	case "", BuildRailpack, BuildAI:
	default:
		return fmt.Errorf("build.builder must be railpack or ai, not %q", b.Builder)
	}
	if b.AI.Attempts < 0 {
		return fmt.Errorf("build.ai.attempts cannot be negative")
	}
	if b.AI.VerifySeconds < 0 {
		return fmt.Errorf("build.ai.verifySeconds cannot be negative")
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

// BuilderOrDefault is the builder an app without a Dockerfile gets.
func (b BuildSettings) BuilderOrDefault() BuildStrategy {
	if b.Builder == "" {
		return BuildRailpack
	}
	return b.Builder
}

// VerifyStartup reports whether a freshly built image is run before it is
// published. On unless an installation turns it off.
func (b BuildSettings) VerifyStartup() bool {
	return b.AI.Verify == nil || *b.AI.Verify
}

// VerifySeconds is how long the app is given to come up, defaulted.
func (b BuildSettings) VerifySeconds() int {
	if b.AI.VerifySeconds <= 0 {
		return DefaultVerifySeconds
	}
	return b.AI.VerifySeconds
}

// AIAttempts is how many Dockerfiles the assistant may write, defaulted.
func (b BuildSettings) AIAttempts() int {
	if b.AI.Attempts <= 0 {
		return DefaultAIAttempts
	}
	return b.AI.Attempts
}

// MergeEnv layers an app's own build.env over the installation's. The app
// wins: a project that pins its own package registry has a reason to, and
// the installation's entry is the fallback for everyone who didn't.
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
