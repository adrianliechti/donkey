package config

import (
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/distribution/reference"
	"sigs.k8s.io/yaml"
)

// InstallFile is the values file for `donkey server install -f server.yaml`:
// everything an admin configures about a donkey installation. Flags override
// individual fields; secrets can come from env instead
// (DONKEY_REGISTRY_PASSWORD, DONKEY_PACKAGES_PASSWORD,
// DONKEY_OIDC_CLIENT_SECRET, OPENAI_API_KEY, EXA_API_KEY).
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
	// BusyboxImage is the runtime toolbox used for file browsing and Docker
	// volume initialization. Set a full image reference for a private mirror.
	BusyboxImage string `json:"busyboxImage,omitempty"`
	// RegistryImage is Docker's optional bundled registry image. It is only
	// used when registry.url is omitted; default registry:3.
	RegistryImage string `json:"registryImage,omitempty"`
	// AddonImages names mirrored repositories for the managed services the
	// runtime pulls itself. Build registry mirrors do not cover these images:
	// they are not part of an app build.
	AddonImages InstallAddonImages `json:"addonImages,omitempty"`
	// AddonImagePullSecrets names pre-existing kubernetes.io/dockerconfigjson
	// secrets used for managed addon and file-helper images. Each secret must
	// exist in every workload namespace where an addon or helper can run.
	AddonImagePullSecrets []string `json:"addonImagePullSecrets,omitempty"`
	// AddonPlugins registers services provisioned by independent gRPC servers.
	AddonPlugins AddonPlugins `json:"addonPlugins,omitempty"`
	// CommonOfferings are available in every space. Empty allows all; space
	// admins can add offerings to a nonempty common catalog in space settings.
	CommonOfferings CommonOfferings `json:"commonOfferings,omitempty"`
	// ProxyServices registers fixed HTTP endpoints as offerings.
	ProxyServices ProxyServices `json:"proxyServices,omitempty"`
	// Domain: apps become <app>.<space>.<domain>.
	Domain string `json:"domain,omitempty"`
	// ServerHost is where the API and console live (default: the domain
	// itself). It must be the domain or a name under it — the central OAuth
	// callback always stays on the domain, and the login cookie's domain has
	// to cover the console too.
	ServerHost string         `json:"serverHost,omitempty"`
	Gateway    InstallGateway `json:"gateway,omitempty"`
	TLS        InstallTLS     `json:"tls,omitempty"`
	// Kubernetes is where donkey's objects go. Only meaningful for the
	// kubernetes runtime.
	Kubernetes InstallKubernetes `json:"kubernetes,omitempty"`
	// DefaultPlatform is the build platform of the cluster's nodes
	// (default linux/amd64).
	DefaultPlatform string `json:"defaultPlatform,omitempty"`

	Build InstallBuild `json:"build,omitempty"`

	Auth     InstallAuth    `json:"auth,omitempty"`
	Platform PlatformConfig `json:"platform,omitempty"`
	AI       InstallAI      `json:"ai,omitempty"`
	Exa      InstallExa     `json:"exa,omitempty"`
}

// InstallAddonImages overrides the image repository of each managed addon.
// Versions remain part of the service configuration and are appended by
// donkey, so these values deliberately contain no tag or digest.
type InstallAddonImages struct {
	Postgres string `json:"postgres,omitempty"`
	Redis    string `json:"redis,omitempty"`
	RustFS   string `json:"rustfs,omitempty"`
}

// Repository returns the configured repository for one addon type.
func (i InstallAddonImages) Repository(typ string) string {
	switch NormalizeAddonType(typ) {
	case "postgres":
		return i.Postgres
	case "redis":
		return i.Redis
	case "s3":
		return i.RustFS
	default:
		return ""
	}
}

// Validate catches values that cannot have an addon version appended to
// them. A registry port is fine; an image tag or digest is not.
func (i InstallAddonImages) Validate() error {
	for _, image := range []struct {
		name       string
		repository string
	}{
		{name: "postgres", repository: i.Postgres},
		{name: "redis", repository: i.Redis},
		{name: "rustfs", repository: i.RustFS},
	} {
		name, repository := image.name, image.repository
		if repository == "" {
			continue
		}
		named, err := reference.ParseNormalizedNamed(repository)
		if err != nil {
			return fmt.Errorf("addonImages.%s %q is not an image repository: %w", name, repository, err)
		}
		if _, tagged := named.(reference.Tagged); tagged {
			return fmt.Errorf("addonImages.%s %q must not include a tag — configure the addon version separately", name, repository)
		}
		if _, digested := named.(reference.Canonical); digested {
			return fmt.Errorf("addonImages.%s %q must not include a digest — configure the addon version separately", name, repository)
		}
	}
	return nil
}

// ValidateAddonImagePullSecrets checks names before the installer writes
// them into pod specs. Secret references are Kubernetes DNS subdomains.
func ValidateAddonImagePullSecrets(names []string) error {
	for _, name := range names {
		if name == "" || len(name) > 253 {
			return fmt.Errorf("addonImagePullSecrets contains invalid secret name %q", name)
		}
		for _, label := range strings.Split(name, ".") {
			if !namespaceRE.MatchString(label) {
				return fmt.Errorf("addonImagePullSecrets contains invalid secret name %q — use lowercase letters, digits, dashes and dots", name)
			}
		}
	}
	return nil
}

// InstallBuild is how apps are built and how a build reaches the outside
// world. BuildSettings travels to donkey-server as environment; mirrors go to
// both the engine config and the server-side session auth provider, and the
// proxy goes to both pods.
type InstallBuild struct {
	BuildSettings `json:",inline"`

	// RegistryMirrors maps an upstream registry host to its mirror. It is
	// written into the build engine's config and shared with its session auth
	// provider, so it covers every FROM a build pulls, including private-CA
	// authenticated mirrors.
	RegistryMirrors map[string]string `json:"registryMirrors,omitempty"`

	// Proxy is a forward proxy for donkey-server and the build engine
	// themselves — not for the app's build steps, which read Env.
	Proxy InstallProxy `json:"proxy,omitempty"`

	// Packages is the upstream side of donkey's always-on, build-only package
	// proxy. Builds only see the internal endpoint; credentials stay here in
	// the control plane.
	Packages InstallPackages `json:"packages,omitempty"`
}

const (
	DefaultNPMUpstream   = "https://registry.npmjs.org"
	DefaultPyPIUpstream  = "https://pypi.org/simple"
	DefaultGoUpstream    = "https://proxy.golang.org"
	DefaultMavenUpstream = "https://repo.maven.apache.org/maven2"
	DefaultNuGetUpstream = "https://api.nuget.org/v3/index.json"
)

// InstallPackages configures the upstreams behind donkey-packages. One
// read-only credential may cover several explicitly named Artifactory/Nexus
// hosts; the proxy never sends it anywhere else.
type InstallPackages struct {
	NPM   string `json:"npm,omitempty"`
	PyPI  string `json:"pypi,omitempty"`
	Go    string `json:"go,omitempty"`
	Maven string `json:"maven,omitempty"`
	NuGet string `json:"nuget,omitempty"`

	// OS package mirrors are opt-in. URLs name archive roots, without an
	// Alpine branch or Debian suite; those come from the build's base image.
	Debian string `json:"debian,omitempty"`
	Alpine string `json:"alpine,omitempty"`

	// ResourceHosts are additional HTTPS hosts an index may send clients to
	// for package metadata or artifacts. They are routed back through
	// donkey-packages but receive no credential unless also named in
	// CredentialHosts.
	ResourceHosts   []string `json:"resourceHosts,omitempty"`
	CredentialHosts []string `json:"credentialHosts,omitempty"`
	Username        string   `json:"username,omitempty"`
	Password        string   `json:"password,omitempty"`

	// DockerConfigSecret is an existing Kubernetes Secret mounted into the
	// package proxy. DockerConfigDir is the equivalent bind-mounted host
	// directory for the Docker runtime. DockerConfigHost selects the auths
	// entry whose credential is reused for the explicitly allowed target hosts
	// in CredentialHosts.
	DockerConfigSecret string `json:"dockerConfigSecret,omitempty"`
	DockerConfigDir    string `json:"dockerConfigDir,omitempty"`
	DockerConfigHost   string `json:"dockerConfigHost,omitempty"`
}

func (p InstallPackages) NPMUpstream() string {
	if p.NPM != "" {
		return p.NPM
	}
	return DefaultNPMUpstream
}

func (p InstallPackages) PyPIUpstream() string {
	if p.PyPI != "" {
		return p.PyPI
	}
	return DefaultPyPIUpstream
}

func (p InstallPackages) GoUpstream() string {
	if p.Go != "" {
		return p.Go
	}
	return DefaultGoUpstream
}

func (p InstallPackages) MavenUpstream() string {
	if p.Maven != "" {
		return p.Maven
	}
	return DefaultMavenUpstream
}

func (p InstallPackages) NuGetUpstream() string {
	if p.NuGet != "" {
		return p.NuGet
	}
	return DefaultNuGetUpstream
}

// Validate checks the public/default or enterprise package endpoints. A
// password may be absent here because an installer re-run can recover it from
// the existing installation Secret; ValidateCredentials performs that final
// check after recovery.
func (p InstallPackages) Validate() error {
	hosts := map[string]map[string]bool{}
	for name, raw := range map[string]string{
		"npm": p.NPMUpstream(), "pypi": p.PyPIUpstream(), "go": p.GoUpstream(),
		"maven": p.MavenUpstream(), "nuget": p.NuGetUpstream(),
		"debian": p.Debian, "alpine": p.Alpine,
	} {
		if raw == "" && (name == "debian" || name == "alpine") {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("build.packages.%s %q must be an http or https URL without embedded credentials", name, raw)
		}
		host := strings.ToLower(u.Host)
		if hosts[host] == nil {
			hosts[host] = map[string]bool{}
		}
		hosts[host][u.Scheme] = true
	}
	resourceHosts := map[string]bool{}
	for _, configured := range p.ResourceHosts {
		host, err := packageHost(configured)
		if err != nil {
			return fmt.Errorf("build.packages.resourceHosts contains %q; entries must be host[:port], not URLs", configured)
		}
		if resourceHosts[host] {
			return fmt.Errorf("build.packages.resourceHosts contains duplicate host %q", configured)
		}
		resourceHosts[host] = true
	}
	credentialHosts := map[string]bool{}
	for _, configured := range p.CredentialHosts {
		host, err := packageHost(configured)
		if err != nil {
			return fmt.Errorf("build.packages.credentialHosts contains %q; entries must be host[:port], not URLs", configured)
		}
		if credentialHosts[host] {
			return fmt.Errorf("build.packages.credentialHosts contains duplicate host %q", configured)
		}
		credentialHosts[host] = true
		if hosts[host] == nil && !resourceHosts[host] {
			return fmt.Errorf("build.packages credential host %q does not match a package upstream or resourceHosts entry", configured)
		}
		if hosts[host] != nil && hosts[host]["http"] {
			return fmt.Errorf("build.packages credential host %q uses plaintext http; credentials require https", configured)
		}
	}
	if p.DockerConfigSecret != "" && p.DockerConfigDir != "" {
		return fmt.Errorf("build.packages must use dockerConfigSecret or dockerConfigDir, not both")
	}
	dockerConfig := p.DockerConfigSecret != "" || p.DockerConfigDir != ""
	if p.DockerConfigHost != "" {
		if _, err := packageHost(p.DockerConfigHost); err != nil {
			return fmt.Errorf("build.packages.dockerConfigHost %q must be host[:port], not a URL", p.DockerConfigHost)
		}
	}
	if dockerConfig && p.DockerConfigHost == "" {
		return fmt.Errorf("build.packages Docker config credentials need dockerConfigHost to select their auths entry")
	}
	if !dockerConfig && p.DockerConfigHost != "" {
		return fmt.Errorf("build.packages.dockerConfigHost needs dockerConfigSecret or dockerConfigDir")
	}
	if dockerConfig && (p.Username != "" || p.Password != "") {
		return fmt.Errorf("build.packages Docker config credentials cannot be combined with username or password")
	}
	if p.Username != "" && len(p.CredentialHosts) == 0 {
		return fmt.Errorf("build.packages.username needs build.packages.credentialHosts to scope the credential")
	}
	if dockerConfig && len(p.CredentialHosts) == 0 {
		return fmt.Errorf("build.packages Docker config credentials need credentialHosts to scope the credential")
	}
	return nil
}

func (p InstallPackages) ValidateCredentials() error {
	if err := p.Validate(); err != nil {
		return err
	}
	dockerConfig := p.DockerConfigSecret != "" || p.DockerConfigDir != ""
	configured := len(p.CredentialHosts) > 0 || p.Username != "" || p.Password != "" || dockerConfig
	if configured && !dockerConfig && (len(p.CredentialHosts) == 0 || p.Username == "" || p.Password == "") {
		return fmt.Errorf("build.packages credentials need credentialHosts, username and password")
	}
	return nil
}

// NeedsPassword reports whether an installer should recover an omitted
// explicit password from the previous installation. Docker config sources
// are mounted and resolved independently.
func (p InstallPackages) NeedsPassword() bool {
	return p.DockerConfigSecret == "" && p.DockerConfigDir == "" &&
		(len(p.CredentialHosts) > 0 || p.Username != "")
}

// ValidateRuntime keeps host paths out of Kubernetes pod specs and
// Kubernetes Secret names out of Docker bind mounts.
func (p InstallPackages) ValidateRuntime(runtime string) error {
	if runtime == "docker" {
		if p.DockerConfigSecret != "" {
			return fmt.Errorf("build.packages.dockerConfigSecret is for Kubernetes; use dockerConfigDir with the Docker runtime")
		}
		if p.DockerConfigDir != "" && !filepath.IsAbs(p.DockerConfigDir) {
			return fmt.Errorf("build.packages.dockerConfigDir must be an absolute host path")
		}
		return nil
	}
	if p.DockerConfigDir != "" {
		return fmt.Errorf("build.packages.dockerConfigDir is for the Docker runtime; use dockerConfigSecret with Kubernetes")
	}
	if p.DockerConfigSecret != "" {
		if err := ValidateAddonImagePullSecrets([]string{p.DockerConfigSecret}); err != nil {
			return fmt.Errorf("build.packages.dockerConfigSecret: %w", err)
		}
	}
	return nil
}

func packageHost(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.Contains(value, "://") || strings.Contains(value, "/") {
		return "", fmt.Errorf("invalid host")
	}
	u, err := url.Parse("https://" + value)
	if err != nil || u.Host != value || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid host")
	}
	if port := u.Port(); port != "" {
		for _, r := range port {
			if r < '0' || r > '9' {
				return "", fmt.Errorf("invalid port")
			}
		}
	}
	return strings.ToLower(value), nil
}

// BuildSettings is the build configuration donkey-server acts on. There is
// one builder, so its settings sit directly under build:.
type BuildSettings struct {
	// Derived from InstallPackages and transported separately to the server.
	// Upstream URLs and credentials never enter the build settings.
	DebianMirror bool `json:"-"`
	AlpineMirror bool `json:"-"`
	// Env is injected into every build, under the app's own build.env.
	// BuildKit hands it to every RUN, which is what puts a package manager's
	// mirror in reach of build steps donkey never sees.
	Env map[string]string `json:"env,omitempty"`
	// Instructions extend the Dockerfile-writing assistant's built-in
	// instructions with installation-specific constraints and hints, such as
	// the image prefixes available on an enterprise registry. They are sent
	// to the configured model, so credentials do not belong here.
	Instructions string `json:"instructions,omitempty"`

	// Verify is a pointer so unset ≠ off; unset means on.
	Verify        *bool `json:"verify,omitempty"`
	VerifySeconds int   `json:"verifySeconds,omitempty"`
	// Attempts is the first Dockerfile plus one revision per failure shown.
	Attempts int `json:"attempts,omitempty"`

	// Provenance selects the SLSA provenance attestation BuildKit attaches
	// to every pushed image: "min" (the default — materials and build
	// metadata, no sources), "max", or "off". Max additionally embeds the
	// Dockerfile and every build-arg *value* into the pushed attestation —
	// never choose it while build.env carries credentials.
	Provenance string `json:"provenance,omitempty"`
	// SBOM attaches an SPDX SBOM attestation to every pushed image. Off by
	// default: it pulls a scanner image (covered by registryMirrors) and
	// lengthens every build.
	SBOM bool `json:"sbom,omitempty"`
}

// ProvenanceMode resolves the attestation tri-state: unset means "min",
// "off" means none. Any other value is refused at validation.
func (b BuildSettings) ProvenanceMode() string {
	switch b.Provenance {
	case "", "min":
		return "min"
	case "max":
		return "max"
	}
	return ""
}

// ValidateProvenance refuses values ProvenanceMode would silently drop.
func (b BuildSettings) ValidateProvenance() error {
	switch b.Provenance {
	case "", "min", "max", "off":
		return nil
	}
	return fmt.Errorf("build.provenance must be min, max or off (got %q)", b.Provenance)
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
	// A full attempt includes build and rollout. Stop earlier if identical
	// inputs repeat the same failure; independent fixes deserve more chances.
	DefaultAttempts = 6
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
		if _, err := NormalizeRegistrySource(host); err != nil {
			return fmt.Errorf("build.registryMirrors source %q is invalid: %w", host, err)
		}
		if _, err := ParseRegistryMirror(mirror); err != nil {
			return fmt.Errorf("build.registryMirrors[%q] mirror %q is invalid: %w", host, mirror, err)
		}
	}
	return b.Packages.Validate()
}

// RegistryMirror is the canonical endpoint used by BuildKit: Address retains
// an optional repository prefix, Host is the host[:port] used for TLS/auth.
type RegistryMirror struct {
	Address   string
	Host      string
	PlainHTTP bool
}

// ParseRegistryMirror accepts the two spellings BuildKit users commonly have
// at hand: host[/repository] and an http(s) URL. Credentials belong in Docker
// config, never in the ConfigMap that holds buildkitd.toml.
func ParseRegistryMirror(raw string) (RegistryMirror, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return RegistryMirror{}, fmt.Errorf("address is empty")
	}
	if !strings.Contains(value, "://") {
		value = "https://" + strings.TrimLeft(value, "/")
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return RegistryMirror{}, fmt.Errorf("expected host[:port] or an http(s) URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return RegistryMirror{}, fmt.Errorf("scheme must be http or https")
	}
	if u.User != nil {
		return RegistryMirror{}, fmt.Errorf("embedded credentials are not allowed")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return RegistryMirror{}, fmt.Errorf("query strings and fragments are not allowed")
	}
	host := strings.ToLower(u.Host)
	path := strings.Trim(u.EscapedPath(), "/")
	address := host
	if path != "" {
		address += "/" + path
	}
	return RegistryMirror{Address: address, Host: host, PlainHTTP: u.Scheme == "http"}, nil
}

// NormalizeRegistrySource returns the exact key BuildKit uses to look up an
// image's source registry. A source is a host only; repository paths belong on
// the mirror side of the mapping.
func NormalizeRegistrySource(raw string) (string, error) {
	endpoint, err := ParseRegistryMirror(raw)
	if err != nil {
		return "", err
	}
	if endpoint.Address != endpoint.Host {
		return "", fmt.Errorf("the source must be a registry host, without a repository path")
	}
	return endpoint.Host, nil
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
	// CASecret is a Secret in the control-plane namespace holding ca.crt.
	CASecret string `json:"caSecret,omitempty"`
	// DockerConfigSecret is an existing Secret whose Docker config entries
	// authenticate separately hosted BuildKit mirrors. The push registry's
	// own credential remains Username plus password/passwordFile.
	DockerConfigSecret string `json:"dockerConfigSecret,omitempty"`
}

func (r InstallRegistry) ValidateRuntime(runtime string) error {
	if runtime == "docker" {
		if r.CASecret != "" {
			return fmt.Errorf("registry.caSecret is for Kubernetes; use registry.caFile with the Docker runtime")
		}
		if r.DockerConfigSecret != "" {
			return fmt.Errorf("registry.dockerConfigSecret is for Kubernetes; use dockerConfigDir with the Docker runtime")
		}
		if r.DockerConfigDir != "" && !filepath.IsAbs(r.DockerConfigDir) {
			return fmt.Errorf("registry.dockerConfigDir must be an absolute host path")
		}
		return nil
	}
	if r.CAFile != "" {
		return fmt.Errorf("registry.caFile is for the Docker runtime; use registry.caSecret with Kubernetes")
	}
	if r.DockerConfigDir != "" {
		return fmt.Errorf("registry.dockerConfigDir is for the Docker runtime; use dockerConfigSecret with Kubernetes")
	}
	if r.DockerConfigSecret != "" {
		if err := ValidateAddonImagePullSecrets([]string{r.DockerConfigSecret}); err != nil {
			return fmt.Errorf("registry.dockerConfigSecret: %w", err)
		}
	}
	if r.CASecret != "" {
		if err := ValidateAddonImagePullSecrets([]string{r.CASecret}); err != nil {
			return fmt.Errorf("registry.caSecret: %w", err)
		}
	}
	return nil
}

// InstallAuth configures the login gateway — the one way in: apps, console
// and CLI all authenticate against this issuer.
type InstallAuth struct {
	Issuer       string `json:"issuer,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
	// Scope is donkey's own API in the IdP (e.g.
	// api://<donkey-client-id>/<scope>), asked for at login on top of
	// openid/profile/email. Logins then keep the IdP's refresh token, and
	// login-protected apps receive the user's access token as
	// X-Auth-Request-Access-Token.
	// The Trino console sends it as Authorization: Bearer to its add-on
	// service, which handles on-behalf-of delegation downstream.
	// Empty: none of that.
	Scope string `json:"scope,omitempty"`
}

// InstallKubernetes says which namespaces donkey may use, and by omission
// which ones it may create.
//
// Both fields follow one rule: name a namespace and it is yours — donkey
// expects it to exist, applies its objects into it, and never creates or
// deletes it. Leave a field empty and donkey makes and manages the namespace
// itself, which is what a cluster you own wants.
type InstallKubernetes struct {
	System   InstallNamespace `json:"system,omitempty"`
	Workload InstallNamespace `json:"workload,omitempty"`
	// IsolateSpaces fences every space with a NetworkPolicy: a space's pods
	// accept traffic only from their own space, the donkey gateway and
	// donkey-server. Cross-space calls keep working through the gateway on an
	// app's public hostname — where login and access rules apply — but stop
	// working as direct pod-to-pod traffic. So does anything else the cluster
	// runs (say, a Prometheus scraping pods across namespaces).
	//
	// A pointer so unset ≠ off: isolation is on by default, and only an
	// explicit `isolateSpaces: false` opens the network between spaces.
	//
	// Enforcement is the CNI's business: on a network plugin that ignores
	// NetworkPolicy the fences apply cleanly and change nothing.
	IsolateSpaces *bool `json:"isolateSpaces,omitempty"`
	// RestrictEgress adds the egress half of the fence: a space's pods reach
	// only their own space, the platform services an app uses (donkey-ai,
	// donkey-proxy, donkey-telemetry) and DNS — never the public internet or another space.
	//
	// Off by default and a plain bool, unlike IsolateSpaces: cutting outbound
	// traffic breaks any app that calls a third-party API at runtime, so it is
	// a deliberate choice, never a default. It needs IsolateSpaces on, since
	// the egress rules ride the same per-space policy.
	RestrictEgress bool `json:"restrictEgress,omitempty"`
}

// SpacesIsolated is on unless an installation turns it off.
func (k InstallKubernetes) SpacesIsolated() bool {
	return k.IsolateSpaces == nil || *k.IsolateSpaces
}

// InstallNamespace names a namespace donkey was given.
type InstallNamespace struct {
	// Namespace is the one to use.
	//
	// Under system: where the control plane lives. It is also the
	// installation's identity, so two installations on one cluster must not
	// share it — and a second one has to set this.
	//
	// Under workload: the single namespace every app of every space lives in.
	// Setting it is what turns a cluster where donkey may not create
	// namespaces into one it can run on, and it is the only arrangement that
	// needs no cluster-scoped permission at all. Apps are then named
	// <space>--<app> on the cluster, which is what lets one namespace hold two
	// spaces that both have an "api".
	//
	// Leaving workload empty gives every space a namespace of its own,
	// <namespacePrefix><space>, created and deleted by donkey.
	Namespace string `json:"namespace,omitempty"`
	// NamespacePrefix maps a space onto its namespace; only read under
	// workload, and only when Namespace is empty (default "donkey-"). Two
	// installations that both give every space a namespace need different
	// prefixes, or they would fight over donkey-<space>.
	NamespacePrefix string `json:"namespacePrefix,omitempty"`
}

// SystemNamespace is where the control plane goes.
func (k InstallKubernetes) SystemNamespace() string {
	if k.System.Namespace != "" {
		return k.System.Namespace
	}
	return DefaultSystemNamespace
}

// NamespacePrefix maps a space onto its own namespace.
func (k InstallKubernetes) NamespacePrefix() string {
	if k.Workload.NamespacePrefix != "" {
		return k.Workload.NamespacePrefix
	}
	return DefaultNamespacePrefix
}

// Validate checks what can be wrong without consulting the cluster.
func (k InstallKubernetes) Validate() error {
	for field, ns := range map[string]string{
		"kubernetes.system.namespace":   k.System.Namespace,
		"kubernetes.workload.namespace": k.Workload.Namespace,
	} {
		if ns != "" && !ValidNamespace(ns) {
			return fmt.Errorf("%s %q is not a valid namespace name — lowercase letters, digits and dashes (max 63 chars)", field, ns)
		}
	}
	// Apps next to the control plane would share a namespace with a
	// privileged build engine and the policy that guards it, and donkey's own
	// objects would be inside the blast radius of every `donkey destroy`.
	if k.Workload.Namespace != "" && k.Workload.Namespace == k.SystemNamespace() {
		return fmt.Errorf("kubernetes.workload.namespace must differ from kubernetes.system.namespace (%s) — apps do not belong next to the control plane", k.SystemNamespace())
	}
	if k.Workload.Namespace != "" && k.Workload.NamespacePrefix != "" {
		return fmt.Errorf("kubernetes.workload.namespacePrefix has no meaning together with kubernetes.workload.namespace — with one namespace for every app there is no per-space namespace to prefix")
	}
	// The two blocks share a shape, but the prefix only means something for
	// app namespaces — accepting it under system: would quietly do nothing.
	if k.System.NamespacePrefix != "" {
		return fmt.Errorf("kubernetes.system.namespacePrefix is not a setting — the prefix belongs under kubernetes.workload")
	}
	// Egress rules ride the space's isolation policy, so there is nowhere to
	// put them when isolation is off.
	if k.RestrictEgress && !k.SpacesIsolated() {
		return fmt.Errorf("kubernetes.restrictEgress needs kubernetes.isolateSpaces — the egress limits ride the same per-space policy the fence creates")
	}
	return nil
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
	// ExternalDNS optionally publishes one exact DNSEndpoint per public
	// hostname. Its targets are normally the static VIP assigned to the
	// donkey-gateway LoadBalancer Service.
	ExternalDNS InstallExternalDNS `json:"externalDNS,omitempty"`
}

// InstallTLS selects where the gateway gets certificates. All modes terminate
// TLS in donkey-gateway and therefore share the same incoming traffic path.
// cert-manager is Kubernetes-only; ACME and the built-in CA work on both
// runtimes.
type InstallTLS struct {
	// Mode is "acme" (the default), "ca", or "cert-manager".
	Mode string `json:"mode,omitempty"`
	// Email and Directory tune ACME issuance.
	Email     string `json:"email,omitempty"`
	Directory string `json:"directory,omitempty"`
	// IssuerName names the cert-manager Issuer or ClusterIssuer used for the
	// control plane and every exposed app.
	IssuerName  string `json:"issuerName,omitempty"`
	IssuerKind  string `json:"issuerKind,omitempty"`
	IssuerGroup string `json:"issuerGroup,omitempty"`
}

// EffectiveMode fills the public ACME default.
func (t InstallTLS) EffectiveMode() string {
	if t.Mode != "" {
		return t.Mode
	}
	return "acme"
}

func (t InstallTLS) UsesCertManager() bool { return t.EffectiveMode() == "cert-manager" }

func (t InstallTLS) CertificateIssuerKind() string {
	if t.IssuerKind != "" {
		return t.IssuerKind
	}
	return "ClusterIssuer"
}

func (t InstallTLS) CertificateIssuerGroup() string {
	if t.IssuerGroup != "" {
		return t.IssuerGroup
	}
	return "cert-manager.io"
}

// Validate checks the TLS block without consulting the cluster.
func (t InstallTLS) Validate() error {
	switch t.Mode {
	case "", "acme", "ca", "cert-manager":
	default:
		return fmt.Errorf("tls.mode must be acme, ca or cert-manager, not %q", t.Mode)
	}
	if t.UsesCertManager() && t.IssuerName == "" {
		return fmt.Errorf("tls.mode cert-manager needs tls.issuerName")
	}
	if !t.UsesCertManager() && (t.IssuerName != "" || t.IssuerKind != "" || t.IssuerGroup != "") {
		return fmt.Errorf("tls issuer settings require tls.mode cert-manager")
	}
	if t.IssuerName != "" && !validKubernetesObjectName(t.IssuerName) {
		return fmt.Errorf("tls.issuerName %q is not a valid Kubernetes object name", t.IssuerName)
	}
	if t.EffectiveMode() != "acme" && (t.Email != "" || t.Directory != "") {
		return fmt.Errorf("tls.email and tls.directory apply only to tls.mode acme")
	}
	return nil
}

// DefaultBuildkitImage is the build engine donkey installs. It is pinned to a
// digest as well as a tag: a tag can be re-pushed, and the build engine runs
// privileged — the one place where "whatever the registry serves today" is
// not good enough. The digest is the multi-platform index of v0.31.2.
const DefaultBuildkitImage = "moby/buildkit:v0.31.2@sha256:2f5adac4ecd194d9f8c10b7b5d7bceb5186853db1b26e5abd3a657af0b7e26ec"

const DefaultBusyboxImage = "busybox:1.37"

const DefaultRegistryImage = "registry:3"

func (f InstallFile) BundledRegistryImage() string {
	if f.RegistryImage != "" {
		return f.RegistryImage
	}
	return DefaultRegistryImage
}

func ValidateBusyboxImage(image string) error {
	return validateRuntimeImage("busyboxImage", image)
}

func ValidateRegistryImage(image string) error {
	return validateRuntimeImage("registryImage", image)
}

func validateRuntimeImage(field, image string) error {
	if image == "" {
		return nil
	}
	if _, err := reference.ParseNormalizedNamed(image); err != nil {
		return fmt.Errorf("%s %q is not an image reference: %w", field, image, err)
	}
	return nil
}

const DefaultExaBaseURL = "https://api.exa.ai"

// InstallExa configures the optional search/contents upstream.
type InstallExa struct {
	BaseURL string `json:"baseUrl,omitempty"`
	APIKey  string `json:"apiKey,omitempty"`
}

func (e InstallExa) Validate() error {
	if e.BaseURL == "" {
		return nil
	}
	u, err := url.Parse(e.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("exa.baseUrl must be an http or https URL without credentials, query or fragment")
	}
	return nil
}

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
